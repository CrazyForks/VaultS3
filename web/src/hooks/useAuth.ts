import { createContext, useContext, useState, useEffect, useCallback, type ReactNode } from 'react'
import { createElement } from 'react'
import {
  login as apiLogin,
  oidcLogin as apiOIDCLogin,
  completeOIDCLogin as apiOIDCComplete,
  getMe,
  type MeResponse,
} from '../api/auth'
import { getToken, setToken, clearToken } from '../api/client'

interface AuthContextType {
  token: string | null
  user: MeResponse | null
  isAuthenticated: boolean
  isLoading: boolean
  login: (accessKey: string, secretKey: string, remember?: boolean) => Promise<void>
  loginWithOIDC: (idToken: string) => Promise<void>
  loginWithOIDCCode: (code: string, state: string) => Promise<void>
  logout: () => void
}

const AuthContext = createContext<AuthContextType | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setTokenState] = useState<string | null>(() => getToken())
  const [user, setUser] = useState<MeResponse | null>(null)
  const [isLoading, setIsLoading] = useState(!!token)

  useEffect(() => {
    if (token) {
      getMe()
        .then(setUser)
        .catch(() => {
          clearToken()
          setTokenState(null)
        })
        .finally(() => setIsLoading(false))
    }
  }, [token])

  const login = useCallback(async (accessKey: string, secretKey: string, remember = false) => {
    const res = await apiLogin(accessKey, secretKey)
    setToken(res.token, remember)
    setTokenState(res.token)
    const me = await getMe()
    setUser(me)
  }, [])

  const loginWithOIDC = useCallback(async (idToken: string) => {
    const res = await apiOIDCLogin(idToken)
    setToken(res.token, true)
    setTokenState(res.token)
    const me = await getMe()
    setUser(me)
  }, [])

  // Authorization-code flow: the server redeems the code, so the page only ever
  // handles the code and the sealed state, never a token or the PKCE verifier.
  const loginWithOIDCCode = useCallback(async (code: string, state: string) => {
    const res = await apiOIDCComplete(code, state)
    setToken(res.token, true)
    setTokenState(res.token)
    const me = await getMe()
    setUser(me)
  }, [])

  const logout = useCallback(() => {
    clearToken()
    setTokenState(null)
    setUser(null)
  }, [])

  return createElement(AuthContext.Provider, {
    value: { token, user, isAuthenticated: !!token && !!user, isLoading, login, loginWithOIDC, loginWithOIDCCode, logout },
    children,
  })
}

export function useAuth(): AuthContextType {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}