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

// Billing
export function createCheckout(tier: string): Promise<{ url: string }> {
  return apiFetch('/api/billing/checkout', {
    method: 'POST',
    body: JSON.stringify({ tier }),
  })
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

// Functions (/bin/)
export function getFunctions(): Promise<ShellFunction[]> {
  return apiFetch('/api/functions')
}

export function saveFunction(name: string, source: string): Promise<void> {
  return apiFetch(`/api/functions/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify({ source }),
  })
}

export function deleteFunction(name: string): Promise<void> {
  return apiFetch(`/api/functions/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

// Watches (/cron.d/)
export interface WatchesResponse {
  watches: CronWatch[]
  count: number
  limit: number
  min_interval: string
}

export function getWatches(): Promise<WatchesResponse> {
  return apiFetch('/api/watches')
}

export function saveWatch(name: string, schedule: string, pipeline: string, notifyDest: string): Promise<void> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify({ schedule, pipeline, notify_dest: notifyDest }),
  })
}

export function deleteWatch(name: string): Promise<void> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

export function getWatchResults(name: string): Promise<CronResult[]> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}/results`)
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

export interface ShellFunction {
  ID: number
  Name: string
  Source: string
  CreatedAt: string
  UpdatedAt: string
}

export interface CronWatch {
  ID: number
  Name: string
  Schedule: string
  Pipeline: string
  NotifyDest: string
  Enabled: boolean
  LastRunAt: string | null
  LastStatus: string
  CreatedAt: string
  UpdatedAt: string
}

export interface CronResult {
  ID: number
  CronWatchID: number
  RanAt: string
  Stdout: string
  Stderr: string
  ExitCode: number
  DurationMs: number
}

// Team
export interface TeamMember {
  ID: string
  CustomerName: string
  Email: string
  UserID: string | null
  Role: 'owner' | 'member'
  Status: 'pending' | 'active' | 'removed'
  InvitedAt: string
  AcceptedAt: string | null
}

export interface TeamResponse {
  members: TeamMember[]
  seats_used: number
  seats_limit: number
  org_name: string
}

export function getTeam(): Promise<TeamResponse> {
  return apiFetch('/api/team')
}

export function inviteMember(email: string): Promise<TeamMember> {
  return apiFetch('/api/team/invite', {
    method: 'POST',
    body: JSON.stringify({ email }),
  })
}

export function removeMember(id: string): Promise<void> {
  return apiFetch(`/api/team/${id}`, { method: 'DELETE' })
}

export function updateTeamName(name: string): Promise<{ name: string }> {
  return apiFetch('/api/team/name', {
    method: 'PUT',
    body: JSON.stringify({ name }),
  })
}

export function acceptInvite(token: string): Promise<{ token: string; user: User }> {
  return apiFetch('/api/team/accept', {
    method: 'POST',
    body: JSON.stringify({ token }),
  })
}

// Integrations
export interface IntegrationsResponse {
  slack: {
    connected: boolean
    team_name: string
    configured_at: string | null
  }
  pagerduty: {
    connected: boolean
    keys: Record<string, string>
  }
  routes: Record<string, string>
}

export function getIntegrations(): Promise<IntegrationsResponse> {
  return apiFetch('/api/integrations')
}

export function connectSlack(): Promise<{ url: string }> {
  return apiFetch('/api/integrations/slack/connect', { method: 'POST' })
}

export function disconnectSlack(): Promise<void> {
  return apiFetch('/api/integrations/slack', { method: 'DELETE' })
}

export function savePagerDutyKey(name: string, key: string): Promise<void> {
  return apiFetch('/api/integrations/pagerduty', {
    method: 'PUT',
    body: JSON.stringify({ name, key }),
  })
}

export function disconnectPagerDuty(): Promise<void> {
  return apiFetch('/api/integrations/pagerduty', { method: 'DELETE' })
}

export function saveRoutes(routes: Record<string, string>): Promise<void> {
  return apiFetch('/api/integrations/routes', {
    method: 'PUT',
    body: JSON.stringify(routes),
  })
}

export { apiFetch }
