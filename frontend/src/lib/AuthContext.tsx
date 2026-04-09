import { createContext, useContext, useState, useEffect, type ReactNode } from 'react'
import { login as apiLogin, verify as apiVerify, getMe, type User } from './api'

interface AuthContextValue {
  user: User | null
  token: string | null
  login: (email: string) => Promise<void>
  verify: (token: string) => Promise<void>
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

  async function verify(tok: string): Promise<void> {
    const { token: jwt, user: u } = await apiVerify(tok)
    localStorage.setItem(TOKEN_KEY, jwt)
    setToken(jwt)
    setUser(u)
  }

  function logout(): void {
    localStorage.removeItem(TOKEN_KEY)
    setToken(null)
    setUser(null)
  }

  return (
    <AuthContext.Provider value={{ user, token, login, verify, logout, loading }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
