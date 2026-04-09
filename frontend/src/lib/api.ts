const API_BASE = import.meta.env.VITE_API_URL || ''

async function apiFetch<T>(path: string, options: RequestInit = {}): Promise<T> {
  const token = localStorage.getItem('weft_token')
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string> || {}),
  }
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  const res = await fetch(`${API_BASE}${path}`, { ...options, headers })

  if (!res.ok) {
    const body = await res.text().catch(() => '')
    throw new Error(body || `HTTP ${res.status}`)
  }

  const text = await res.text()
  return (text ? JSON.parse(text) : null) as T
}

// Auth
export function login(email: string): Promise<{ message: string }> {
  return apiFetch('/api/auth/login', {
    method: 'POST',
    body: JSON.stringify({ email }),
  })
}

export function verify(token: string): Promise<{ token: string; user: User }> {
  return apiFetch('/api/auth/verify', {
    method: 'POST',
    body: JSON.stringify({ token }),
  })
}

export function getMe(): Promise<User> {
  return apiFetch('/api/auth/me')
}

// Usage
export function getUsage(): Promise<Usage> {
  return apiFetch('/api/usage')
}

// Tokens
export function getTokens(): Promise<AgentToken[]> {
  return apiFetch('/api/tokens')
}

export function createToken(label: string, app: string): Promise<AgentToken & { secret: string }> {
  return apiFetch('/api/tokens', {
    method: 'POST',
    body: JSON.stringify({ label, app }),
  })
}

export function revokeToken(token: string): Promise<void> {
  return apiFetch(`/api/tokens/${token}`, { method: 'DELETE' })
}

// Queries
export function getQueries(): Promise<SavedQuery[]> {
  return apiFetch('/api/queries')
}

export function createQuery(name: string, query: string): Promise<SavedQuery> {
  return apiFetch('/api/queries', {
    method: 'POST',
    body: JSON.stringify({ name, query }),
  })
}

export function deleteQuery(id: string): Promise<void> {
  return apiFetch(`/api/queries/${id}`, { method: 'DELETE' })
}

// Types
export interface User {
  id: string
  email: string
  name: string
  tier: string
  customer: string
}

export interface Usage {
  tier: string
  bytes_ingested: number
  tier_limit_bytes: number
  period_start: string
}

export interface AgentToken {
  token: string
  label: string
  app: string
  status: 'active' | 'revoked'
  created_at: string
}

export interface SavedQuery {
  id: string
  name: string
  query: string
  created_at: string
}

export { apiFetch }
