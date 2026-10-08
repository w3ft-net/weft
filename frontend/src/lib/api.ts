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

// localStorage keys are exported so AuthContext / login flow can clear
// or rewrite them without re-stringing the names. Keep both in sync —
// orphaned weft_org entries against a logged-out token are harmless
// but read confusingly in devtools.
export const TOKEN_STORAGE_KEY = 'weft_token'
export const ORG_STORAGE_KEY = 'weft_org'

async function apiFetch<T>(path: string, options: RequestInit = {}): Promise<T> {
  const token = localStorage.getItem(TOKEN_STORAGE_KEY)
  const org = localStorage.getItem(ORG_STORAGE_KEY)
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string> || {}),
  }
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }
  // X-Active-Org pins this request to a specific membership. Absent →
  // server falls back to last_active_customer_id, then oldest membership.
  if (org) {
    headers['X-Active-Org'] = org
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
export function getIngestionStatus(): Promise<{ affected_services: string[] }> {
  return apiFetch('/api/ingestion-status')
}

// Tokens
export function getTokens(): Promise<AgentToken[]> {
  return apiFetch('/api/tokens')
}

export function createToken(label: string, namespace: string, env: string, producer?: string): Promise<AgentToken & { token: string }> {
  return apiFetch('/api/tokens', {
    method: 'POST',
    body: JSON.stringify({ label, namespace, env, producer: producer || undefined }),
  })
}

export function revokeToken(id: number): Promise<void> {
  return apiFetch(`/api/tokens/${id}`, { method: 'DELETE' })
}

// Registered ingest/syslog.Producer platforms (e.g. Render) a token can
// be minted `--for`. Drives the producer selector on the token-create
// form instead of hardcoding platform names — see docs/ingestion-syslog.md.
export interface SyslogProducer {
  name: string
  endpoint: string
}

export function getSyslogProducers(): Promise<SyslogProducer[]> {
  return apiFetch('/api/syslog-producers')
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

// SaveWatchResult.warnings carries non-blocking, save-time messages
// (e.g. a cron whose tail/head cap risks silently truncating data) —
// the save always succeeds; warnings is empty/absent when there's
// nothing to say. Generic by design: any future save-time check uses
// the same field, so callers don't need to know which check fired.
export interface SaveWatchResult {
  warnings?: string[]
}

export function saveWatch(name: string, schedule: string, pipeline: string): Promise<SaveWatchResult> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify({ schedule, pipeline }),
  })
}

export function deleteWatch(name: string): Promise<void> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

export function getWatch(name: string): Promise<CronWatch> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}`)
}

export function getWatchResults(name: string): Promise<CronResult[]> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}/results`)
}

export type ChartRange = '1h' | '6h' | '24h' | '7d' | '30d' | '90d' | '1y'

export const CHART_RANGES: readonly ChartRange[] = ['1h', '6h', '24h', '7d', '30d', '90d', '1y'] as const

// Approximate widths in ms — used client-side to grey out range
// buttons whose windows exceed the caller's plan retention. Server
// is still authoritative (it rejects over-long ranges), this is just
// UX.
export const CHART_RANGE_MS: Record<ChartRange, number> = {
  '1h':  60 * 60 * 1000,
  '6h':  6 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d':  7 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
  '90d': 90 * 24 * 60 * 60 * 1000,
  '1y':  365 * 24 * 60 * 60 * 1000,
}

export interface ChartPoint {
  ts: number // unix millis
  tag: string
  value: number
  count: number
  min: number
  max: number
}

export interface ChartResponse {
  points: ChartPoint[]
  granularity: 'raw' | 'hourly' | '4h' | 'daily'
  range: ChartRange
}

export function getWatchChart(name: string, range: ChartRange): Promise<ChartResponse> {
  return apiFetch(`/api/watches/${encodeURIComponent(name)}/chart?range=${range}`)
}

// resendVerification re-sends the signup verification email to the
// currently-authenticated user. Returns {status: "sent"} on success, or
// {status: "already_verified"} for users who've already completed the
// round-trip.
export function resendVerification(): Promise<{ status: string }> {
  return apiFetch('/api/auth/resend-verification', { method: 'POST' })
}

// Types
export interface OrgMembership {
  id: string    // org UUID
  name: string
  role: string  // "admin" (the org's owner) or "member"
  active: boolean
}

export interface User {
  id: string
  email: string
  name: string
  tier: string
  customer: string  // active org's slug — same as the entry in orgs[] where active=true
  orgs: OrgMembership[]
  email_verified?: boolean
  verification_deadline?: number // unix seconds; present only when unverified
  has_seen_welcome?: boolean
  // Onboarding state. 'welcome' is the default for new signups;
  // 'done' marks a graduate (membership in Loom Inc has been revoked).
  // The UI keys the welcome modal + tour overlays off these.
  onboarding_step?: string
  onboarding_dismissed?: boolean
  // onboarding_prompt is the lesson copy for shell-* steps, rendered
  // server-side so the lesson list lives in one place. Empty for
  // non-shell steps (panel-tour copy is hardcoded in the frontend
  // because it references UI element names we want to keep close to
  // the components). onboarding_action is the suggested command for
  // the current step; Shell.tsx injects it into the input buffer.
  onboarding_prompt?: string
  onboarding_action?: string
  // pending_graduation_notice is true between tour completion
  // (onboarding_step === 'done') and the user dismissing the
  // graduation banner. Gates GraduationBanner so the success message
  // shows exactly once per account.
  pending_graduation_notice?: boolean
  is_superadmin?: boolean
}

// Flips the authenticated user's has_seen_welcome flag to true. Call
// on welcome-modal dismiss so the modal doesn't re-appear on the next
// login. Server-side is idempotent.
export function dismissWelcome(): Promise<void> {
  return apiFetch('/api/account/welcome-dismiss', { method: 'POST' })
}

// advanceOnboarding bumps onboarding_step from `expected` to the
// next step in the server-side sequence. The server returns the new
// step (or the current step, unchanged, on race). When the new step
// is 'done', the server has already revoked Loom membership and
// dropped a graduation notice — callers should refresh the user
// record to pick up the new orgs[] list.
export function advanceOnboarding(expected: string): Promise<{ step: string; advanced: boolean }> {
  return apiFetch('/api/onboarding/advance', {
    method: 'POST',
    body: JSON.stringify({ step: expected }),
  })
}

// dismissOnboarding flips onboarding_dismissed=true so the modal +
// tour overlays go quiet. Doesn't revoke Loom membership; the user
// can still browse via the org switcher.
export function dismissOnboarding(): Promise<void> {
  return apiFetch('/api/onboarding/dismiss', { method: 'POST' })
}

// dismissGraduationNotice marks the one-shot post-tour success
// banner as seen so subsequent /me responses set
// pending_graduation_notice=false. Idempotent.
export function dismissGraduationNotice(): Promise<void> {
  return apiFetch('/api/onboarding/graduation-seen', { method: 'POST' })
}

// FSEntry mirrors api.FSEntry on the server: one row in a directory
// listing returned by /api/fs/list, used by the shell chrome's
// tree sidebars.
export interface FSEntry {
  name: string
  is_dir: boolean
}

// listFS returns the immediate children of a path. Empty `entries`
// is a real result (an empty directory) — distinct from a 404 which
// throws.
export function listFS(path: string): Promise<{ entries: FSEntry[] }> {
  return apiFetch(`/api/fs/list?path=${encodeURIComponent(path)}`)
}

export interface Usage {
  tier: string
  bytes_ingested: number
  tier_limit_bytes: number
  period_start: string
  // Cents the customer would owe on the next invoice if ingestion
  // stopped now. Zero under the tier's free quota or on tiers without
  // an overage rate (trial). Stripe is authoritative for the real
  // billable number; this is a dashboard heads-up.
  projected_overage_cents: number
  metrics_forecast?: {
    agents_with_metrics: number
    projected_bytes_month: number
  }
}

export interface AgentToken {
  id: number
  prefix: string
  label: string
  namespace: string
  env: string
  status: 'active' | 'revoked'
  created_at: string
  producer?: string
  endpoint?: string
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
  // Alert projection — populated only when the watch's `alert` stage
  // uses a condition with a single numeric cutoff (above/below/eq/neq).
  // Rising/falling/changed/absent leave both null.
  AlertCondition: 'above' | 'below' | 'eq' | 'neq' | null
  AlertThreshold: number | null
}

export interface CronResult {
  ID: number
  CronWatchID: number
  RanAt: string
  Stdout: string
  Stderr: string
  ExitCode: number
  DurationMs: number
  // Parsed numeric value from the watch's alert_key field (or the whole
  // stdout in single-row mode). Null when the value wasn't parseable.
  Value: number | null
  // Per-row tag from the watch's alert_tag field. Empty in single-row mode.
  Tag: string
}

// Team
export interface TeamMember {
  ID: string
  CustomerName: string
  Email: string
  UserID: string | null
  Role: 'admin' | 'member'   // 'admin' is the org's owner
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

// Org permissions

export type AccessLevel = 'edit' | 'read-only'

export interface NamespaceAcl {
  read: boolean
  create_tokens?: boolean
}

export interface MemberPerms {
  bin: AccessLevel
  cron: AccessLevel
  etc: AccessLevel
  namespaces?: Record<string, NamespaceAcl>
}

// PermissionsDoc keys are member emails. Owners are not represented
// (they have implicit full access); non-owners missing from the doc
// have default-deny (all read-only, no namespace access).
export type PermissionsDoc = Record<string, MemberPerms>

export function getPermissions(): Promise<PermissionsDoc> {
  return apiFetch('/api/permissions')
}

export function setPermissions(doc: PermissionsDoc): Promise<void> {
  return apiFetch('/api/permissions', {
    method: 'PUT',
    body: JSON.stringify(doc),
  })
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
  JSONPath: string
  Sample: string
}

// FieldCandidate is the shape shared by level, trace ID, and all four
// span fields (mirrors Go's ingest.FieldCandidate). Exactly one of
// FieldIndex/JSONPath is meaningful for a given candidate — FieldIndex 0
// with a non-empty Pattern and no JSONPath means "search the whole line"
// (Schema's TraceIDPattern convention).
export interface FieldCandidate {
  FieldIndex: number
  JSONPath: string
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
  LevelCandidates: FieldCandidate[] | null
  TraceIDCandidates: FieldCandidate[] | null
  SpanNameCandidates: FieldCandidate[] | null
  SpanIDCandidates: FieldCandidate[] | null
  ParentSpanIDCandidates: FieldCandidate[] | null
  DurationNsCandidates: FieldCandidate[] | null
}

export interface PendingSource {
  service: string
  namespace: string
  instance: string
  host_group: string
  sample: string[]
  detection: SourceDetection
  created_at: string
  // "sampling" = still accumulating (not yet configurable); "pending" =
  // full sample, ready to configure.
  status: 'sampling' | 'pending'
  sample_collected: number
  sample_target: number
}

export interface ConfigureSourceRequest {
  format: string
  timestamp_field: string
  timestamp_format: string
  level_field: string
  level_pattern: string
  trace_id_field: string
  trace_id_pattern: string
  span_name_field: string
  span_name_pattern: string
  span_id_field: string
  span_id_pattern: string
  parent_span_id_field: string
  parent_span_id_pattern: string
  duration_ns_field: string
  duration_ns_pattern: string
  expand_keys?: { key: string; type: string }[]
}

export async function getPendingSources(namespace?: string): Promise<PendingSource[]> {
  const path = namespace
    ? `/api/sources/pending?namespace=${encodeURIComponent(namespace)}`
    : '/api/sources/pending'
  const resp = await apiFetch<PendingSource[] | null>(path)
  return resp || []
}

// getPendingSourceCount is the cheap form of getPendingSources().length, for
// the sidebar badge that polls every 30s.
export async function getPendingSourceCount(): Promise<number> {
  const resp = await apiFetch<{ count: number }>('/api/sources/pending/count')
  return resp.count
}

export function configureSource(
  namespace: string,
  service: string,
  config: ConfigureSourceRequest,
): Promise<void> {
  return apiFetch(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(service)}/configure`,
    { method: 'POST', body: JSON.stringify(config) },
  )
}

// fieldRef formats a FieldCandidate's location the way Schema's own
// *Field strings expect: a JSON path, a 1-based positional index, or
// "none" for a whole-line pattern search (FieldIndex 0 with no JSONPath —
// NOT "0", which parse.go's extractors treat as a real, always-out-of-
// range positional index and would silently never match).
function fieldRef(c: FieldCandidate | undefined): { field: string; pattern: string } {
  if (!c) return { field: '', pattern: '' }
  if (c.JSONPath) return { field: c.JSONPath, pattern: c.Pattern }
  if (c.FieldIndex >= 1) return { field: String(c.FieldIndex), pattern: c.Pattern }
  return { field: 'none', pattern: c.Pattern }
}

// Best-guess defaults from the server's Detection. Level, trace ID, and
// the span fields are now populated from the classifier's top candidate
// the same way timestamp already is — earlier versions left them blank
// because the old pattern-matching heuristic wasn't reliable enough to
// trust blindly, but every candidate here has already been validated
// against the real sample (see ingest/llmdetect.go's validateClassification)
// before it ever reaches this response.
export function suggestFromDetection(d: SourceDetection): ConfigureSourceRequest {
  const format =
    d.JSONCoverage > 0.8 ? 'json'
      : d.JSONTailCoverage > 0.5 ? 'prefix_json'
        : 'text'
  const ts = d.TimestampCandidates?.[0]
  const level = fieldRef(d.LevelCandidates?.[0])
  const traceID = fieldRef(d.TraceIDCandidates?.[0])
  const spanName = fieldRef(d.SpanNameCandidates?.[0])
  const spanID = fieldRef(d.SpanIDCandidates?.[0])
  const parentSpanID = fieldRef(d.ParentSpanIDCandidates?.[0])
  const durationNs = fieldRef(d.DurationNsCandidates?.[0])
  return {
    format,
    timestamp_field: ts ? (ts.JSONPath || String(ts.FieldIndex)) : '',
    timestamp_format: ts?.Format ?? '',
    level_field: level.field,
    level_pattern: level.pattern,
    trace_id_field: traceID.field,
    trace_id_pattern: traceID.pattern,
    span_name_field: spanName.field,
    span_name_pattern: spanName.pattern,
    span_id_field: spanID.field,
    span_id_pattern: spanID.pattern,
    parent_span_id_field: parentSpanID.field,
    parent_span_id_pattern: parentSpanID.pattern,
    duration_ns_field: durationNs.field,
    duration_ns_pattern: durationNs.pattern,
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

// ─── Configured sources (CloudWatch push + weftd) ────────────────

export interface SourceInfo {
  namespace: string
  service: string
  source_type: 'weftd' | 'kinesis' | 'direct'
  log_group: string
  region: string
  enabled: boolean
}

export async function listSources(namespace?: string): Promise<SourceInfo[]> {
  const path = namespace
    ? `/api/sources?namespace=${encodeURIComponent(namespace)}`
    : '/api/sources'
  const resp = await apiFetch<SourceInfo[] | null>(path)
  return resp || []
}

// SourceSchema is the GET /api/sources/{ns}/{service}/schema response. For a
// configured source, schema_yaml carries the live schema (viewable at any
// time). For a still-sampling source, sample_collected/sample_target
// report progress so the UI can say "not enough lines yet (N/target)".
export interface SourceSchema {
  status: 'configured' | 'sampling' | 'pending'
  schema_yaml?: string
  schema_hash?: string
  resample_requested?: boolean
  sample_collected?: number
  sample_target?: number
}

export function getSourceSchema(namespace: string, service: string): Promise<SourceSchema> {
  return apiFetch<SourceSchema>(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(service)}/schema`,
  )
}

// resampleSource asks the agent for a fresh sample of an already-configured
// source. Ingestion continues on the current schema; the fresh sample
// appears for review once the agent finishes accumulating.
export function resampleSource(namespace: string, service: string): Promise<void> {
  return apiFetch(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(service)}/resample`,
    { method: 'POST' },
  )
}

export function deleteSource(namespace: string, service: string): Promise<void> {
  // service is the bare service name (e.g. "coordinator"); it maps
  // directly to the positional path segment the backend expects.
  return apiFetch(
    `/api/sources/${encodeURIComponent(namespace)}/${encodeURIComponent(service)}`,
    { method: 'DELETE' },
  )
}

// ─── Kinesis (Firehose push) source add flow ─────────────────────
//
// Unlike CloudWatch poll mode, there's no "pull sample" step: the source
// is created pending, and CloudWatchAdapter accumulates a real sample
// from the customer's own Firehose deliveries once their CloudFormation
// stack is live. It then shows up in getPendingSources() like any other
// pending source, and finishes through the existing
// <PendingSourceConfigure> flow.

export interface AddKinesisSourceRequest {
  namespace: string
  service: string
  log_group: string
  region: string
}

export function addKinesisSource(req: AddKinesisSourceRequest): Promise<void> {
  return apiFetch('/api/sources/kinesis', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

// Dashboards (/etc/dashboards/)
export type DashboardTileSize = 'sm' | 'md' | 'lg'
export type DashboardTileKind = 'sparkline' | 'counter' | 'gauge' | 'table'

export interface DashboardThreshold {
  warn?: number
  crit?: number
  max?: number
}

export interface DashboardTile {
  kind: DashboardTileKind
  watch: string
  size?: DashboardTileSize
  format?: string
  threshold?: DashboardThreshold
  columns?: string[]
}

export interface DashboardSection {
  title?: string
  tiles: DashboardTile[]
}

export interface DashboardSpec {
  title?: string
  sections: DashboardSection[]
}

export interface DashboardWatchPayload {
  watch: CronWatch
  results: CronResult[]
}

export interface DashboardListItem {
  name: string
  title?: string
  updated_at?: string
  invalid?: boolean
}

export interface DashboardResponse {
  name: string
  updated_at?: string
  spec: DashboardSpec
  watches: Record<string, DashboardWatchPayload | null>
}

export async function listDashboards(): Promise<DashboardListItem[]> {
  const resp = await apiFetch<{ dashboards: DashboardListItem[] }>('/api/dashboards')
  return resp.dashboards || []
}

export function getDashboard(name: string): Promise<DashboardResponse> {
  return apiFetch(`/api/dashboards/${encodeURIComponent(name)}`)
}

// ── Admin API ─────────────────────────────────────────────────────────────

export interface AdminCustomer {
  id: string    // org UUID
  name: string
  display_name: string
  tier: string
  is_comp: boolean
  comp_reason?: string
  status: string
  created_at: string
}

export interface AdminCustomerDetail {
  id: string    // org UUID
  name: string
  display_name: string
  namespaces: { id: number; name: string }[]
  team: { email: string; role: string; status: string }[]
  created_at: string
  subscription?: {
    id: number
    tier: string
    status: string
    is_comp: boolean
    comp_reason: string
    starts_at: string
  }
}

export interface AdminBucket {
  ID: number
  // Go store.Bucket.Services is a []string with no json tag, so it
  // serializes as a JSON array under the capitalized key. Empty array
  // ([]) is the catch-all bucket.
  Services: string[]
  WindowDuration: string
  TargetIngestingCount: number
  Tier: string
  CreatedAt: string
}

export interface AdminShard {
  ID: string
  WindowStart: string
  WindowEnd: string
  State: string
  Addr: string
  InstanceID: string
  ParquetPath: string
  Tier: string
  RowCount: number
  CreatedAt: string
  BucketID: number
}

export interface AdminCreatedCustomer {
  name: string
  tier: string
  comp_reason: string
  owner_email: string
  // Magic link for the owner's first login. Emailed best-effort by the
  // server; returned here so the operator can deliver it directly.
  login_url: string
}

export function adminCreateCustomer(
  email: string, orgName: string, tier: string, reason?: string,
): Promise<AdminCreatedCustomer> {
  return apiFetch('/api/admin/customers', {
    method: 'POST',
    body: JSON.stringify({ email, org_name: orgName, tier, reason }),
  })
}

export function adminListCustomers(): Promise<AdminCustomer[]> {
  return apiFetch('/api/admin/customers')
}

export function adminGetCustomer(name: string): Promise<AdminCustomerDetail> {
  return apiFetch(`/api/admin/customers/${encodeURIComponent(name)}`)
}

export function adminSetComp(name: string, comp: boolean, tier?: string, reason?: string): Promise<void> {
  return apiFetch(`/api/admin/customers/${encodeURIComponent(name)}/comp`, {
    method: 'POST',
    body: JSON.stringify({ comp, tier, reason }),
  })
}

export function adminSetTier(name: string, tier: string): Promise<void> {
  return apiFetch(`/api/admin/customers/${encodeURIComponent(name)}/tier`, {
    method: 'POST',
    body: JSON.stringify({ tier }),
  })
}

export function adminListBuckets(): Promise<AdminBucket[]> {
  return apiFetch('/api/admin/buckets')
}

export function adminGetBucket(id: number): Promise<AdminBucket> {
  return apiFetch(`/api/admin/buckets/${id}`)
}

export function adminUpdateBucket(id: number, updates: Partial<Pick<AdminBucket, 'WindowDuration' | 'TargetIngestingCount' | 'Tier'>>): Promise<AdminBucket> {
  return apiFetch(`/api/admin/buckets/${id}`, {
    method: 'PUT',
    body: JSON.stringify({
      window_duration: updates.WindowDuration,
      target_ingesting_count: updates.TargetIngestingCount,
      tier: updates.Tier,
    }),
  })
}

export function adminListShards(bucketId: number): Promise<AdminShard[]> {
  return apiFetch(`/api/admin/buckets/${bucketId}/shards`)
}

export function adminArchiveShard(shardId: string): Promise<void> {
  return apiFetch(`/api/admin/shards/${encodeURIComponent(shardId)}/archive`, {
    method: 'POST',
  })
}

export function adminDeleteShard(shardId: string, force?: boolean): Promise<void> {
  return apiFetch(`/api/admin/shards/${encodeURIComponent(shardId)}`, {
    method: 'DELETE',
    body: JSON.stringify({ force: !!force }),
  })
}

export { apiFetch }
