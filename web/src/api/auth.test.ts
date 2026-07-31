import { describe, it, expect } from 'vitest'
import { buildOIDCAuthorizeUrl, type OIDCConfigResponse } from './auth'

// The URLs from issue #44, as an Authentik deployment actually presents them.
const AUTHENTIK: OIDCConfigResponse = {
  enabled: true,
  issuerUrl: 'https://authentik.example.com/application/o/my-app',
  clientId: 'my-client-id',
  authorizeUrl: 'https://authentik.example.com/application/o/authorize/',
}

const REDIRECT = 'https://s3.example.com/dashboard/oidc-callback'

describe('buildOIDCAuthorizeUrl', () => {
  it('sends the user to the endpoint the provider published', () => {
    const url = buildOIDCAuthorizeUrl(AUTHENTIK, REDIRECT, 'abc123')
    expect(url.startsWith('https://authentik.example.com/application/o/authorize/?')).toBe(true)
  })

  it('never builds the issuer-derived URL that 404s on Authentik', () => {
    const url = buildOIDCAuthorizeUrl(AUTHENTIK, REDIRECT, 'abc123')
    // This is exactly the URL the old code produced, and it does not exist.
    expect(url).not.toContain('/application/o/my-app/authorize')
  })

  it('carries the parameters the implicit flow needs', () => {
    const url = new URL(buildOIDCAuthorizeUrl(AUTHENTIK, REDIRECT, 'nonce-42'))
    expect(url.searchParams.get('response_type')).toBe('id_token')
    expect(url.searchParams.get('client_id')).toBe('my-client-id')
    expect(url.searchParams.get('redirect_uri')).toBe(REDIRECT)
    expect(url.searchParams.get('nonce')).toBe('nonce-42')
    expect(url.searchParams.get('response_mode')).toBe('fragment')
    expect(url.searchParams.get('scope')).toBe('openid email profile')
  })

  it('falls back to the issuer-derived URL when the provider publishes no endpoint', () => {
    const legacy: OIDCConfigResponse = { ...AUTHENTIK, authorizeUrl: undefined }
    const url = buildOIDCAuthorizeUrl(legacy, REDIRECT, 'abc123')
    expect(url.startsWith('https://authentik.example.com/application/o/my-app/authorize?')).toBe(true)
  })

  it('appends to an endpoint that already has query parameters', () => {
    const withQuery: OIDCConfigResponse = {
      ...AUTHENTIK,
      authorizeUrl: 'https://idp.example.com/authorize?tenant=acme',
    }
    const url = buildOIDCAuthorizeUrl(withQuery, REDIRECT, 'abc123')
    expect(url).toContain('?tenant=acme&response_type=id_token')
    expect(new URL(url).searchParams.get('tenant')).toBe('acme')
    expect(new URL(url).searchParams.get('response_type')).toBe('id_token')
  })

  it('escapes values that would otherwise break the query string', () => {
    const cfg: OIDCConfigResponse = { ...AUTHENTIK, clientId: 'client id&x=1' }
    const url = new URL(buildOIDCAuthorizeUrl(cfg, REDIRECT, 'n&x=2'))
    expect(url.searchParams.get('client_id')).toBe('client id&x=1')
    expect(url.searchParams.get('nonce')).toBe('n&x=2')
  })
})
