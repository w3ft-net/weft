const API_BASE = import.meta.env.VITE_API_URL || ''

// ApiError surfaces structured codes from the backend (e.g.
// email_verification_required) so callers can render the right UI
// affordance instead of a generic alert. Plain string errors still fall
// into message; code is "" when the backend returned non-JSON text.
export class ApiError extends Error {
  code: string
  status: number
  constructor(message: string, code: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

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
    // Try to parse as a structured error. Backend emits
    // {code, message} for things the UI should react to (e.g.
    // email_verification_required); plain text otherwise.
    let code = ''
    let message = body || `HTTP ${res.status}`
    try {
      const parsed = JSON.parse(body)
      if (parsed && typeof parsed === 'object' && parsed.code) {
        code = String(parsed.code)
        message = String(parsed.message || body)
      }
    } catch {
      // not JSON — leave defaults
    }
    throw new ApiError(message, code, res.status)
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

export function signup(email: string, orgName: string, tier: string): Promise<{ token: string; tier: string; user: User }> {
  return apiFetch('/api/auth/signup', {
    method: 'POST',
    body: JSON.stringify({ email, org_name: orgName, tier }),
  })
}

export function verify(token: string, orgName?: string): Promise<{ token?: string; user?: User; needs_org?: boolean; email?: string }> {
  return apiFetch('/api/auth/verify', {
    method: 'POST',
    body: JSON.stringify({ token, org_name: orgName }),
  })
}

export function getMe(): Promise<User> {
  return apiFetch('/api/auth/me')
}

// Billing
export function createSetupIntent(tier: string): Promise<{ client_secret: string; publishable_key: string }> {
  return apiFetch('/api/billing/setup-intent', {
    method: 'POST',
    body: JSON.stringify({ tier }),
  })
}

export function changeSubscription(tier: string): Promise<{ status?: string; requires_action?: boolean; client_secret?: string; publishable_key?: string }> {
  return apiFetch('/api/billing/checkout', {
    method: 'POST',
    body: JSON.stringify({ tier }),
  })
}

// Usage
export function getUsage(): Promise<Usage> {
  return apiFetch('/api/usage')
}

// Ingestion status
export function getIngestionStatus(): Promise<{ affected_apps: string[] }> {
  return apiFetch('/api/ingestion-status')
}

// Tokens
export function getTokens(): Promise<AgentToken[]> {
  return apiFetch('/api/tokens')
}

export function createToken(label: string, namespace: string): Promise<AgentToken & { token: string }> {
  return apiFetch('/api/tokens', {
    method: 'POST',
    body: JSON.stringify({ label, namespace }),
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
export async function getFunctions(): Promise<ShellFunction[]> {
  const resp = await apiFetch<{ functions: ShellFunction[] }>('/api/functions')
  return resp.functions || []
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

export function saveWatch(name: string, schedule: string, pipeline: string): Promise<void> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify({ schedule, pipeline }),
  })
}

export function deleteWatch(name: string): Promise<void> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

export function getWatchResults(name: string): Promise<CronResult[]> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}/results`)
}

// resendVerification re-sends the signup verification email to the
// currently-authenticated user. Returns {status: "sent"} on success, or
// {status: "already_verified"} for users who've already completed the
// round-trip.
export function resendVerification(): Promise<{ status: string }> {
  return apiFetch('/api/auth/resend-verification', { method: 'POST' })
}

// Types
export interface User {
  id: string
  email: string
  name: string
  tier: string
  customer: string
  email_verified?: boolean
  verification_deadline?: number // unix seconds; present only when unverified
}

export interface Usage {
  tier: string
  bytes_ingested: number
  tier_limit_bytes: number
  period_start: string
  metrics_forecast?: {
    agents_with_metrics: number
    projected_bytes_month: number
  }
}

export interface AgentToken {
  token: string
  label: string
  namespace: string
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
export interface PagerDutyIntegrationMeta {
  name: string
  last_used_at: string | null
  last_error: string
}

export interface IntegrationsResponse {
  slack: {
    connected: boolean
    team_name: string
    configured_at: string | null
  }
  pagerduty: {
    connected: boolean
    keys: Record<string, string>
    integrations: PagerDutyIntegrationMeta[]
  }
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

export function deletePagerDutyKey(name: string): Promise<void> {
  return apiFetch(`/api/integrations/pagerduty/${encodeURIComponent(name)}`, {
    method: 'DELETE',
  })
}

export function disconnectPagerDuty(): Promise<void> {
  return apiFetch('/api/integrations/pagerduty', { method: 'DELETE' })
}

// Namespaces
export interface NamespaceInfo {
  name: string
  id: number
}

export async function getNamespaces(): Promise<NamespaceInfo[]> {
  const resp = await apiFetch<{ namespaces: NamespaceInfo[] }>('/api/namespaces')
  return resp.namespaces || []
}

export function createNamespace(name: string): Promise<NamespaceInfo> {
  return apiFetch('/api/namespaces', {
    method: 'POST',
    body: JSON.stringify({ name }),
  })
}

export interface NamespacePermission {
  can_read: boolean
  can_create_tokens: boolean
  can_manage_watches: boolean
  can_manage_functions: boolean
  can_manage_integrations: boolean
}

export function setNamespacePermission(
  nsName: string, userId: string, perms: NamespacePermission
): Promise<void> {
  return apiFetch(`/api/namespaces/${encodeURIComponent(nsName)}/permissions/${userId}`, {
    method: 'PUT',
    body: JSON.stringify(perms),
  })
}

// Pending sources (weftd auto-discovery)

// Field names mirror Go's Detection struct (no json tags — PascalCase).
export interface TimestampCandidate {
  FieldIndex: number
  SpanFields: number
  Format: string
  Sample: string
}

export interface LevelCandidate {
  FieldIndex: number
  Sample: string
  Pattern: string
}

export interface SourceDetection {
  Lines: string[] | null
  HasKevconsEnvelope: boolean
  StrippedLines: string[] | null
  Instances: string[] | null
  JSONCoverage: number
  JSONTailCoverage: number
  JSONTailStartField: number
  TimestampCandidates: TimestampCandidate[] | null
  LevelCandidates: LevelCandidate[] | null
}

export interface PendingSource {
  app: string
  namespace: string
  instance: string
  host_group: string
  sample: string[]
  detection: SourceDetection
  created_at: string
}

export interface ConfigureSourceRequest {
  format: string
  timestamp_field: string
  timestamp_format: string
  level_field: string
  level_pattern: string
  expand_keys?: { key: string; type: string }[]
}

export async function getPendingSources(namespace?: string): Promise<PendingSource[]> {
  const path = namespace
    ? `/api/sources/pending?namespace=${encodeURIComponent(namespace)}`
    : '/api/sources/pending'
  const resp = await apiFetch<PendingSource[] | null>(path)
  return resp || []
}

export function configureSource(
  namespace: string,
  app: string,
  config: ConfigureSourceRequest,
): Promise<void> {
  return apiFetch(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(app)}/configure`,
    { method: 'POST', body: JSON.stringify(config) },
  )
}

// Best-guess defaults from the server's Detection. Level fields are left
// blank on purpose — we don't surface level extraction in the UI since
// most real logs don't carry a level on every line.
export function suggestFromDetection(d: SourceDetection): ConfigureSourceRequest {
  const format =
    d.JSONCoverage > 0.8 ? 'json'
      : d.JSONTailCoverage > 0.5 ? 'prefix_json'
        : 'text'
  const ts = d.TimestampCandidates?.[0]
  return {
    format,
    timestamp_field: ts ? String(ts.FieldIndex) : '',
    timestamp_format: ts?.Format ?? '',
    level_field: '',
    level_pattern: '',
  }
}

// Cross-page change notifications for pending sources. The sidebar badge
// and the Namespaces page both need to refresh when a source transitions
// from pending → configured (or a new one arrives via weftd).
const pendingSourcesBus = new EventTarget()

export function notifyPendingSourcesChanged(): void {
  pendingSourcesBus.dispatchEvent(new Event('change'))
}

export function onPendingSourcesChanged(handler: () => void): () => void {
  pendingSourcesBus.addEventListener('change', handler)
  return () => pendingSourcesBus.removeEventListener('change', handler)
}

// ─── Configured sources (CloudWatch + weftd) ─────────────────────

export interface SourceInfo {
  namespace: string
  app: string
  source_type: 'cloudwatch' | 'weftd'
  log_group: string
  region: string
  poll_interval: string
  enabled: boolean
  last_poll_at: string
  aws_account_id?: string
}

export async function listSources(namespace?: string): Promise<SourceInfo[]> {
  const path = namespace
    ? `/api/sources?namespace=${encodeURIComponent(namespace)}`
    : '/api/sources'
  const resp = await apiFetch<SourceInfo[] | null>(path)
  return resp || []
}

export function deleteSource(namespace: string, app: string): Promise<void> {
  // The backend stores the full app as "{namespace}/{app}" but its DELETE
  // path takes just the sub-app, so strip the prefix if present.
  const sub = app.startsWith(namespace + '/') ? app.slice(namespace.length + 1) : app
  return apiFetch(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(sub)}`,
    { method: 'DELETE' },
  )
}

// ─── CloudWatch source add flow ──────────────────────────────────

export interface CloudWatchCreds {
  namespace: string
  app: string
  aws_account_id: string
  role_arn: string
  external_id: string
  log_group: string
  region: string
}

export interface CloudWatchSampleResponse {
  lines: string[]
  detection: SourceDetection
}

export async function sampleCloudWatchSource(
  creds: Omit<CloudWatchCreds, 'app'>,
): Promise<CloudWatchSampleResponse> {
  return apiFetch<CloudWatchSampleResponse>('/api/sources/cloudwatch/sample', {
    method: 'POST',
    body: JSON.stringify(creds),
  })
}

export interface AddCloudWatchSourceRequest extends CloudWatchCreds {
  poll_interval: string
  format: string
  timestamp_field: string
  timestamp_format: string
  level_field: string
  level_pattern: string
  expand_keys?: { key: string; type: string }[]
}

export function addCloudWatchSource(req: AddCloudWatchSourceRequest): Promise<void> {
  return apiFetch('/api/sources/cloudwatch', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

export { apiFetch }
