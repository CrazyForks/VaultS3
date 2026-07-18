package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the React SPA from embedded files.
// Static files (js, css, images) are served directly.
// All other paths fall back to index.html for client-side routing.
//
// basePath is an optional reverse-proxy subpath (e.g. "/vaults3") under which the
// whole app is hosted (issue #36). When set — or when the request carries an
// X-Forwarded-Prefix header — the served index.html has its asset URLs and a
// runtime base global rewritten so the browser requests
// <base>/dashboard/assets/... and the SPA router / API client use <base>. Empty =
// today's behavior (served at /dashboard).
func Handler(basePath string) http.Handler {
	dist, _ := fs.Sub(distFS, "dist")
	fileServer := http.FileServer(http.FS(dist))
	indexHTML, _ := fs.ReadFile(dist, "index.html")
	configBase := sanitizeBase(basePath)

	return http.StripPrefix("/dashboard", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := r.URL.Path

		// Normalize: strip trailing slashes to prevent redirect loops
		if urlPath != "/" && strings.HasSuffix(urlPath, "/") {
			urlPath = strings.TrimRight(urlPath, "/")
			r.URL.Path = urlPath
		}

		// Base path: explicit config wins; otherwise honor the proxy's forwarded
		// prefix so it works with zero config behind a well-behaved reverse proxy.
		base := configBase
		if base == "" {
			base = sanitizeBase(r.Header.Get("X-Forwarded-Prefix"))
		}

		if urlPath == "" || urlPath == "/" {
			serveIndex(w, indexHTML, base)
			return
		}

		// Check if the file exists in the embedded filesystem
		cleanPath := strings.TrimPrefix(urlPath, "/")
		if _, err := fs.Stat(dist, cleanPath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// If path has a file extension, it's a missing asset — return 404
		if path.Ext(urlPath) != "" {
			http.NotFound(w, r)
			return
		}

		// SPA fallback: serve index.html directly for client-side routes
		serveIndex(w, indexHTML, base)
	}))
}

// serveIndex writes index.html, rewriting asset URLs to include the base subpath
// and injecting the runtime base global the frontend reads.
func serveIndex(w http.ResponseWriter, indexHTML []byte, base string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(renderIndex(indexHTML, base))
}

// renderIndex rewrites the embedded index.html for a given base subpath. base is
// already sanitized (safe path chars, no trailing slash), so it's safe in an HTML
// attribute. base == "" leaves an empty base meta.
func renderIndex(indexHTML []byte, base string) []byte {
	s := string(indexHTML)
	if base != "" {
		// Built asset refs look like href="/dashboard/..." / src="/dashboard/...";
		// prefix them with the base so the browser requests <base>/dashboard/...
		s = strings.ReplaceAll(s, `"/dashboard/`, `"`+base+`/dashboard/`)
	}
	// Expose the runtime base the SPA reads for its router basename + API base as a
	// <meta> tag rather than an inline <script>: the dashboard's own CSP is
	// `default-src 'self'` with no script-src 'unsafe-inline', so an inline script
	// would be blocked (issue #36). A meta tag is not executable, so it's exempt.
	s = strings.Replace(s, "<head>",
		"<head>\n    <meta name=\"vaults3-base\" content=\""+base+"\">", 1)
	return []byte(s)
}

// sanitizeBase normalizes a base subpath and strips anything that isn't a safe
// path character, so a client-supplied X-Forwarded-Prefix can't inject markup
// into the served HTML. Returns "" for empty/"/" or if nothing safe remains.
func sanitizeBase(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	var b strings.Builder
	for _, c := range p {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '-' || c == '_' || c == '.' {
			b.WriteRune(c)
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	out = strings.TrimRight(out, "/")
	if out == "" {
		return ""
	}
	return out
}
