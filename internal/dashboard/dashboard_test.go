package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardHandler exercises the SPA routing rules against the embedded
// filesystem: root and extension-less routes fall back to index.html (200),
// while a missing asset with a file extension returns 404.
func TestDashboardHandler(t *testing.T) {
	h := Handler("")

	t.Run("root serves html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/", nil))
		if rec.Code != 200 {
			t.Fatalf("root status=%d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("root content-type=%q, want text/html", ct)
		}
	})

	t.Run("spa route falls back to index", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/buckets/my-bucket", nil))
		if rec.Code != 200 {
			t.Fatalf("spa route status=%d, want 200 (index.html fallback)", rec.Code)
		}
	})

	t.Run("missing asset returns 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/nonexistent-asset-xyz.js", nil))
		if rec.Code != 404 {
			t.Fatalf("missing asset status=%d, want 404", rec.Code)
		}
	})

	t.Run("no base: assets stay at /dashboard/ and base global is empty", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/", nil))
		body := rec.Body.String()
		if !strings.Contains(body, `"/dashboard/assets/`) {
			t.Fatalf("expected unprefixed asset URLs, got: %s", body)
		}
		if !strings.Contains(body, `<meta name="vaults3-base" content="">`) {
			t.Fatalf("expected empty base meta, got: %s", body)
		}
	})

	t.Run("configured base rewrites assets + base global", func(t *testing.T) {
		hb := Handler("/vaults3")
		rec := httptest.NewRecorder()
		hb.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard/", nil))
		body := rec.Body.String()
		if !strings.Contains(body, `"/vaults3/dashboard/assets/`) {
			t.Fatalf("assets not prefixed with base: %s", body)
		}
		if !strings.Contains(body, `<meta name="vaults3-base" content="/vaults3">`) {
			t.Fatalf("base meta not set: %s", body)
		}
	})

	t.Run("X-Forwarded-Prefix auto-detected when no config", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/dashboard/", nil)
		req.Header.Set("X-Forwarded-Prefix", "/proxied/")
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, `"/proxied/dashboard/assets/`) {
			t.Fatalf("forwarded prefix not applied: %s", body)
		}
		if !strings.Contains(body, `<meta name="vaults3-base" content="/proxied">`) {
			t.Fatalf("base meta from header not set: %s", body)
		}
	})

	t.Run("malicious forwarded prefix is sanitized (no markup injection)", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/dashboard/", nil)
		req.Header.Set("X-Forwarded-Prefix", `/x"><script>alert(1)</script>`)
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Fatalf("unsanitized header injected markup: %s", body)
		}
	})
}
