import { useEffect, useState } from 'react'

/**
 * Where the identity provider sends the user back.
 *
 * Two shapes arrive here. The authorization-code flow returns `code` and `state`
 * as query parameters, which are handed to the opener for the server to redeem.
 * The legacy implicit flow returns an `id_token` in the URL fragment. An error
 * from the provider (declined consent, a misconfigured client) is forwarded too,
 * so the login page can say what went wrong instead of waiting forever.
 */
export default function OIDCCallbackPage() {
  const [message, setMessage] = useState('Completing sign in...')

  useEffect(() => {
    const query = new URLSearchParams(window.location.search)
    const fragment = new URLSearchParams(window.location.hash.substring(1))

    const providerError = query.get('error') || fragment.get('error')
    const code = query.get('code')
    const state = query.get('state')
    const idToken = fragment.get('id_token')

    if (!window.opener) {
      setMessage('This page completes an SSO sign-in and cannot be opened directly.')
      return
    }

    const send = (data: Record<string, string>) => {
      window.opener.postMessage({ type: 'oidc-callback', ...data }, window.location.origin)
      window.close()
    }

    if (providerError) {
      const description = query.get('error_description') || fragment.get('error_description')
      send({ error: description || providerError })
      return
    }
    if (code && state) {
      send({ code, state })
      return
    }
    if (idToken) {
      send({ idToken })
      return
    }
    setMessage('The identity provider returned no sign-in result.')
  }, [])

  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-50 dark:bg-gray-900">
      <p className="text-gray-500 dark:text-gray-400 text-sm">{message}</p>
    </div>
  )
}
