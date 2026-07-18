// Runtime base path for hosting the dashboard under a reverse-proxy subpath
// (issue #36). The Go server injects `<meta name="vaults3-base" content="...">`
// into index.html from `server.base_path` / `VAULTS3_BASE_PATH` or the
// `X-Forwarded-Prefix` header. A meta tag (not an inline script) is used because
// the dashboard's CSP blocks inline scripts. Empty by default → unchanged.
function normalize(raw: unknown): string {
  let p = typeof raw === 'string' ? raw.trim() : ''
  if (!p || p === '/') return ''
  if (!p.startsWith('/')) p = '/' + p
  return p.replace(/\/+$/, '') // no trailing slash
}

function readBase(): string {
  if (typeof document === 'undefined') return ''
  return document.querySelector('meta[name="vaults3-base"]')?.getAttribute('content') || ''
}

export const BASE_PATH = normalize(readBase())

// The dashboard SPA lives at <base>/dashboard, the admin API at <base>/api/v1.
export const DASHBOARD_BASE = `${BASE_PATH}/dashboard`
export const API_BASE = `${BASE_PATH}/api/v1`
