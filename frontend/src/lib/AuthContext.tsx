import { createContext, useContext, useState, useEffect, type ReactNode } from 'react'
import { login as apiLogin, verify as apiVerify, getMe, type User } from './api'

interface AuthContextValue {
  user: User | null
  token: string | null
  login: (email: string) => Promise<void>
  verify: (token: string, orgName?: string) => Promise<{ needs_org?: boolean; email?: string }>
  loginWithToken: (jwt: string) => Promise<void>
  logout: () => void
  loading: boolean
}

const AuthContext = createContext<AuthContextValue | null>(null)

const TOKEN_KEY = 'weft_token'

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [token, setToken] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    const stored = localStorage.getItem(TOKEN_KEY)
    if (stored) {
      setToken(stored)
      getMe()
        .then(u => setUser(u))
        .catch(() => {
          localStorage.removeItem(TOKEN_KEY)
          setToken(null)
        })
        .finally(() => setLoading(false))
    } else {
      setLoading(false)
    }
  }, [])

  async function login(email: string): Promise<void> {
    await apiLogin(email)
  }

  async function verify(tok: string, orgName?: string): Promise<{ needs_org?: boolean; email?: string }> {
    const resp = await apiVerify(tok, orgName)
    if (resp.needs_org) {
      return { needs_org: true, email: resp.email }
    }
    if (resp.token) {
      localStorage.setItem(TOKEN_KEY, resp.token)
      setToken(resp.token)
      if (resp.user) setUser(resp.user)
    }
    return {}
  }

  async function loginWithToken(jwt: string): Promise<void> {
    localStorage.setItem(TOKEN_KEY, jwt)
    setToken(jwt)
    const u = await getMe()
    setUser(u)
  }

  function logout(): void {
    localStorage.removeItem(TOKEN_KEY)
    setToken(null)
    setUser(null)
  }

  return (
    <AuthContext.Provider value={{ user, token, login, verify, loginWithToken, logout, loading }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
