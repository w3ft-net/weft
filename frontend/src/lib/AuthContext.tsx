import { createContext, useContext, useState, useEffect, type ReactNode } from 'react'
import {
  login as apiLogin,
  verify as apiVerify,
  getMe,
  ORG_STORAGE_KEY,
  TOKEN_STORAGE_KEY,
  type User,
} from './api'

interface AuthContextValue {
  user: User | null
  token: string | null
  login: (email: string) => Promise<void>
  verify: (token: string, orgName?: string) => Promise<{ needs_org?: boolean; email?: string }>
  loginWithToken: (jwt: string) => Promise<void>
  logout: () => void
  // switchOrg writes the new org to localStorage and reloads the page so
  // every cached query (dashboards, watches, sources) is re-fetched
  // against the new active customer. A pure refetch without reload
  // would leave stale data on screens the user already opened.
  switchOrg: (orgName: string) => void
  // refreshUser re-fetches /api/auth/me without reloading the page.
  // Used by TourOverlay to pick up onboarding step changes the
  // shell-side hook drove (advancing through lessons) so the banner
  // updates without forcing a navigation.
  refreshUser: () => Promise<void>
  loading: boolean
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [token, setToken] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    const stored = localStorage.getItem(TOKEN_STORAGE_KEY)
    if (stored) {
      setToken(stored)
      getMe()
        .then(u => setUser(u))
        .catch(() => {
          localStorage.removeItem(TOKEN_STORAGE_KEY)
          localStorage.removeItem(ORG_STORAGE_KEY)
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
      localStorage.setItem(TOKEN_STORAGE_KEY, resp.token)
      setToken(resp.token)
      if (resp.user) setUser(resp.user)
    }
    return {}
  }

  async function loginWithToken(jwt: string): Promise<void> {
    localStorage.setItem(TOKEN_STORAGE_KEY, jwt)
    setToken(jwt)
    const u = await getMe()
    setUser(u)
  }

  function logout(): void {
    localStorage.removeItem(TOKEN_STORAGE_KEY)
    localStorage.removeItem(ORG_STORAGE_KEY)
    setToken(null)
    setUser(null)
  }

  function switchOrg(orgName: string): void {
    localStorage.setItem(ORG_STORAGE_KEY, orgName)
    // Hard reload — every page in the app caches data against the
    // previously active org; refetching just /api/auth/me would leave
    // stale dashboards / watches / sources visible.
    window.location.reload()
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
    <AuthContext.Provider value={{ user, token, login, verify, loginWithToken, logout, switchOrg, refreshUser, loading }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
