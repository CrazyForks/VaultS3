import { describe, it, expect, beforeEach, vi } from 'vitest'
import { getToken, setToken, clearToken, apiFetch } from './client'

// A minimal in-memory Web Storage so the test does not depend on a DOM env.
function createStorage(): Storage {
  const m = new Map<string, string>()
  return {
    get length() { return m.size },
    clear: () => m.clear(),
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    key: (i: number) => Array.from(m.keys())[i] ?? null,
    removeItem: (k: string) => { m.delete(k) },
    setItem: (k: string, v: string) => { m.set(k, String(v)) },
  }
}

// The token store backs the "remember me" login option: remember=true persists
// the session token across browser restarts (localStorage), remember=false keeps
// it only for the tab session (sessionStorage). These tests pin that behavior
// since it is a security-relevant choice.
describe('token storage', () => {
  beforeEach(() => {
    globalThis.localStorage = createStorage()
    globalThis.sessionStorage = createStorage()
  })

  it('remember=true persists to localStorage only', () => {
    setToken('tok-abc', true)
    expect(localStorage.getItem('vaults3_token')).toBe('tok-abc')
    expect(sessionStorage.getItem('vaults3_token')).toBeNull()
    expect(getToken()).toBe('tok-abc')
  })

  it('remember=false keeps the token in sessionStorage only', () => {
    setToken('tok-xyz', false)
    expect(sessionStorage.getItem('vaults3_token')).toBe('tok-xyz')
    expect(localStorage.getItem('vaults3_token')).toBeNull()
    expect(getToken()).toBe('tok-xyz')
  })

  it('switching remember off moves the token and leaves no persistent copy', () => {
    setToken('tok-1', true)
    setToken('tok-1', false)
    expect(localStorage.getItem('vaults3_token')).toBeNull()
    expect(sessionStorage.getItem('vaults3_token')).toBe('tok-1')
  })

  it('getToken prefers the persistent token when both stores hold one', () => {
    localStorage.setItem('vaults3_token', 'persistent')
    sessionStorage.setItem('vaults3_token', 'session')
    expect(getToken()).toBe('persistent')
  })

  it('clearToken removes the token from both stores', () => {
    localStorage.setItem('vaults3_token', 'a')
    sessionStorage.setItem('vaults3_token', 'b')
    clearToken()
    expect(getToken()).toBeNull()
    expect(localStorage.getItem('vaults3_token')).toBeNull()
    expect(sessionStorage.getItem('vaults3_token')).toBeNull()
  })

  it('getToken returns null when nothing is stored', () => {
    expect(getToken()).toBeNull()
  })
})

// Regression for issue #23: attach-policy / add-to-group return a success status
// with an EMPTY body. apiFetch must not call res.json() on an empty body (that
// throws a SyntaxError, surfaced by WebKit as "The string did not match the
// expected pattern"), which broke those dashboard actions on Safari.
describe('apiFetch empty-body handling', () => {
  beforeEach(() => {
    globalThis.localStorage = createStorage()
    globalThis.sessionStorage = createStorage()
  })

  it('resolves undefined for a 200 with an empty body (does not throw)', async () => {
    globalThis.fetch = vi.fn(async () => new Response('', { status: 200 })) as unknown as typeof fetch
    await expect(apiFetch('/iam/users/admin/policies', { method: 'POST' })).resolves.toBeUndefined()
  })

  it('resolves undefined for a 204 No Content', async () => {
    globalThis.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch
    await expect(apiFetch('/iam/users/admin/policies/p', { method: 'DELETE' })).resolves.toBeUndefined()
  })

  it('still parses a JSON body on a 200 with content', async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({ name: 'admin' }), { status: 200 })) as unknown as typeof fetch
    await expect(apiFetch('/iam/users/admin')).resolves.toEqual({ name: 'admin' })
  })

  it('throws the server error message on a non-ok response', async () => {
    globalThis.fetch = vi.fn(async () => new Response(JSON.stringify({ error: 'policy not found' }), { status: 404 })) as unknown as typeof fetch
    await expect(apiFetch('/iam/users/admin/policies', { method: 'POST' })).rejects.toThrow('policy not found')
  })
})
