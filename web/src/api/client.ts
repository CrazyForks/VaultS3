import { API_BASE, DASHBOARD_BASE } from '../basePath'

const BASE = API_BASE
const TOKEN_KEY = 'vaults3_token'
const REMEMBER_KEY = 'vaults3_remember_access_key'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY) || sessionStorage.getItem(TOKEN_KEY)
}

// Remembered credential: only the access key is persisted (never the secret), so
// "Remember me" pre-fills the login form on the next visit. Storing the secret in
// the browser would be a needless exposure for a self-hosted admin console.
export function getRememberedAccessKey(): string {
  return localStorage.getItem(REMEMBER_KEY) || ''
}

export function setRememberedAccessKey(accessKey: string): void {
  if (accessKey) {
    localStorage.setItem(REMEMBER_KEY, accessKey)
  } else {
    localStorage.removeItem(REMEMBER_KEY)
  }
}

export function clearRememberedAccessKey(): void {
  localStorage.removeItem(REMEMBER_KEY)
}

export function setToken(token: string, remember: boolean): void {
  if (remember) {
    localStorage.setItem(TOKEN_KEY, token)
    sessionStorage.removeItem(TOKEN_KEY)
  } else {
    sessionStorage.setItem(TOKEN_KEY, token)
    localStorage.removeItem(TOKEN_KEY)
  }
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
  sessionStorage.removeItem(TOKEN_KEY)
}

export async function apiFetch<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const token = getToken()
  const res = await fetch(`${BASE}${path}`, {
    ...opts,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(opts.headers as Record<string, string>),
    },
  })
  if (res.status === 401) {
    clearToken()
    window.location.href = `${DASHBOARD_BASE}/login`
    throw new Error('Unauthorized')
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(body.error || res.statusText)
  }
  // Tolerate empty success bodies (e.g. 204, or a 200 with no content from an
  // action endpoint). Calling res.json() on an empty body throws a SyntaxError
  // (which WebKit surfaces as "The string did not match the expected pattern").
  if (res.status === 204) return undefined as T
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}