import { createContext, useContext, useState, useEffect, type ReactNode } from 'react'
import {
  login as apiLogin,
  verify as apiVerify,
  getMe,
  TOKEN_STORAGE_KEY,
  type User,
} from './api'

interface AuthContextValue {
  user: User | null
  token: string | null
  login: (email: string) => Promise<void>
  // Resolves to the plan the link's signup chose, if it was a signup link.
  verify: (token: string) => Promise<{ signupTier?: string }>
  loginWithToken: (jwt: string) => Promise<void>
  logout: () => void
  // refreshUser re-fetches /api/auth/me without reloading the page, for an
  // extension whose state lives on the user record.
  refreshUser: () => Promise<void>
  // True when the api authenticates with its one Basic credential: the browser
  // answers that challenge itself, so there is no sign-in page and no sign out.
  open: boolean
  loading: boolean
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [token, setToken] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    // Ask who we are whether or not a session token is stored: an open api has
    // no token to hold and answers with session "open".
    const stored = localStorage.getItem(TOKEN_STORAGE_KEY)
    if (stored) setToken(stored)
    getMe()
      .then(u => setUser(u))
      .catch(() => {
        if (stored) {
          localStorage.removeItem(TOKEN_STORAGE_KEY)
          setToken(null)
        }
      })
      .finally(() => setLoading(false))
  }, [])

  async function login(email: string): Promise<void> {
    await apiLogin(email)
  }

  async function verify(tok: string): Promise<{ signupTier?: string }> {
    const resp = await apiVerify(tok)
    localStorage.setItem(TOKEN_STORAGE_KEY, resp.token)
    setToken(resp.token)
    setUser(resp.user)
    return { signupTier: resp.signup_tier || undefined }
  }

  async function loginWithToken(jwt: string): Promise<void> {
    localStorage.setItem(TOKEN_STORAGE_KEY, jwt)
    setToken(jwt)
    const u = await getMe()
    setUser(u)
  }

  function logout(): void {
    localStorage.removeItem(TOKEN_STORAGE_KEY)
    setToken(null)
    setUser(null)
  }

  async function refreshUser(): Promise<void> {
    try {
      const u = await getMe()
      setUser(u)
    } catch (e) {
      // Best effort: a transient network error shouldn't blow up the
      // app. The next refresh tick will catch it.
      console.error('refreshUser:', e)
    }
  }

  return (
    <AuthContext.Provider value={{ user, token, login, verify, loginWithToken, logout, refreshUser, open: user?.session === 'open', loading }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
