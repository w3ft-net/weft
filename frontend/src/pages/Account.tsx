import { useState, useEffect, Fragment, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { loadStripe, type Stripe } from '@stripe/stripe-js'
import { Elements, PaymentElement, useStripe, useElements } from '@stripe/react-stripe-js'
import SidebarNav from '../components/SidebarNav'
import {
  getUsage, changeSubscription,
  getTeam, inviteMember, removeMember, updateTeamName,
  getIntegrations, connectSlack, disconnectSlack, savePagerDutyKey, disconnectPagerDuty,
  getPermissions, setPermissions, getNamespaces,
  type Usage, type TeamResponse, type IntegrationsResponse,
  type PermissionsDoc, type MemberPerms, type AccessLevel, type NamespaceInfo,
} from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import './Dashboard.css'

const ELEMENTS_APPEARANCE = {
  theme: 'night' as const,
  variables: {
    colorPrimary: '#c9a96e',
    colorBackground: '#141414',
    colorText: '#e8e6e3',
    colorDanger: '#d4635a',
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    borderRadius: '4px',
  },
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
  })
}

// ── Plan tiers ─────────────────────────────────────────────────────────────

const PLANS = [
  { tier: 'swatch', name: 'Swatch', price: 19, volume: '10 GB/mo', retention: '30 days', overage: '$2/GB' },
  { tier: 'yard', name: 'Yard', price: 79, volume: '100 GB/mo', retention: '90 days', overage: '$1/GB' },
  { tier: 'bolt', name: 'Bolt', price: 299, volume: '500 GB/mo', retention: '180 days', overage: '$0.75/GB' },
]

// ── Upgrade confirmation (Stripe Elements) ─────────────────────────────────

function ConfirmForm({ onDone }: { onDone: (err?: string) => void }) {
  const stripe = useStripe()
  const elements = useElements()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!stripe || !elements) return
    setSubmitting(true)
    setError('')
    const result = await stripe.confirmPayment({
      elements,
      confirmParams: { return_url: `${window.location.origin}/account` },
      redirect: 'if_required',
    })
    if (result.error) {
      setError(result.error.message ?? 'Payment failed.')
      setSubmitting(false)
      return
    }
    onDone()
  }

  return (
    <form onSubmit={handleSubmit} data-testid="account-confirm-payment-form">
      <PaymentElement options={{ layout: 'tabs' }} />
      {error && <div className="login-error" data-testid="account-confirm-error">{error}</div>}
      <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
        <button
          type="submit"
          className="btn-action btn-action-primary"
          disabled={!stripe || submitting}
          data-testid="account-confirm-submit-button"
        >
          {submitting ? 'Confirming...' : 'Confirm payment'}
        </button>
        <button
          type="button"
          className="btn-action"
          onClick={() => onDone('cancelled')}
          data-testid="account-confirm-cancel-button"
        >
          Cancel
        </button>
      </div>
    </form>
  )
}

// ── Subscription Card ──────────────────────────────────────────────────────

function SubscriptionCard({ usage, onRefresh }: { usage: Usage | null; onRefresh: () => void }) {
  const navigate = useNavigate()
  const [upgrading, setUpgrading] = useState<string | null>(null)
  const [confirm, setConfirm] = useState<{ stripePromise: Promise<Stripe | null>; clientSecret: string } | null>(null)
  const [error, setError] = useState('')

  async function handleUpgrade(tier: string) {
    setError('')
    const currentTier = usage?.tier?.toLowerCase() || 'trial'
    // Trial users who never completed card entry go through signup/payment.
    if (currentTier === 'trial') {
      navigate(`/signup/payment?tier=${encodeURIComponent(tier)}`)
      return
    }
    setUpgrading(tier)
    try {
      const resp = await changeSubscription(tier)
      if (resp.requires_action && resp.client_secret && resp.publishable_key) {
        setConfirm({
          stripePromise: loadStripe(resp.publishable_key),
          clientSecret: resp.client_secret,
        })
      } else {
        onRefresh()
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Upgrade failed.')
    } finally {
      setUpgrading(null)
    }
  }

  const currentTier = usage?.tier?.toLowerCase() || 'trial'
  const isTrial = currentTier === 'trial'
  const pct = usage
    ? Math.min(100, Math.round((usage.bytes_ingested / usage.tier_limit_bytes) * 100))
    : 0
  const barColor = pct > 90 ? 'var(--red)' : pct > 70 ? 'var(--yellow)' : 'var(--green)'

  return (
    <div className="dash-card" data-testid="account-subscription-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Subscription</h2>
        {usage && <span className="dash-tier-badge" data-testid="account-tier-badge">{usage.tier}</span>}
      </div>

      {/* Trial banner */}
      {isTrial && (
        <div className="trial-banner" data-testid="account-trial-banner">
          You're on a free trial. Choose a plan below to continue after the trial ends.
        </div>
      )}

      {error && (
        <div className="login-error" data-testid="account-subscription-error" style={{ marginBottom: 12 }}>
          {error}
        </div>
      )}

      {confirm && (
        <div className="dash-card" style={{ marginBottom: 16, borderColor: 'var(--accent)' }} data-testid="account-confirm-modal">
          <div className="dash-card-header">
            <h3 className="dash-card-title">Confirm payment</h3>
          </div>
          <Elements stripe={confirm.stripePromise} options={{ clientSecret: confirm.clientSecret, appearance: ELEMENTS_APPEARANCE }}>
            <ConfirmForm onDone={(err) => {
              setConfirm(null)
              if (!err) onRefresh()
            }} />
          </Elements>
        </div>
      )}

      {/* Current usage */}
      {usage && (
        <>
          <div className="usage-stats">
            <div className="usage-stat">
              <div className="usage-stat-label">Ingested this period</div>
              <div className="usage-stat-value" data-testid="account-bytes-ingested">
                {formatBytes(usage.bytes_ingested)}
              </div>
            </div>
            <div className="usage-stat">
              <div className="usage-stat-label">Tier limit</div>
              <div className="usage-stat-value">{formatBytes(usage.tier_limit_bytes)}</div>
            </div>
            <div className="usage-stat">
              <div className="usage-stat-label">Period start</div>
              <div className="usage-stat-value">{formatDate(usage.period_start)}</div>
            </div>
          </div>
          <div className="usage-bar-wrap" style={{ marginBottom: usage.projected_overage_cents > 0 ? 8 : 24 }}>
            <div className="usage-bar-track" data-testid="account-usage-bar">
              <div className="usage-bar-fill" style={{ width: `${pct}%`, background: barColor }} />
            </div>
            <span className="usage-bar-pct" style={{ color: barColor }}>{pct}%</span>
          </div>
          {usage.projected_overage_cents > 0 && (
            <div
              className="usage-projected-overage"
              data-testid="account-projected-overage"
              style={{ marginBottom: 24 }}
            >
              Projected overage this period:{' '}
              <strong>${(usage.projected_overage_cents / 100).toFixed(2)}</strong>
              <span className="usage-projected-overage-hint">
                {' '}— billed at period end.
              </span>
            </div>
          )}

          {/* Metrics forecast */}
          {usage.metrics_forecast && usage.metrics_forecast.agents_with_metrics > 0 && (
            <div className="usage-stats" style={{ marginBottom: 20 }} data-testid="account-metrics-forecast">
              <div className="usage-stat">
                <div className="usage-stat-label">Metrics agents</div>
                <div className="usage-stat-value">{usage.metrics_forecast.agents_with_metrics}</div>
              </div>
              <div className="usage-stat">
                <div className="usage-stat-label">Projected metrics</div>
                <div className="usage-stat-value">~{formatBytes(usage.metrics_forecast.projected_bytes_month)}/mo</div>
              </div>
              <div className="usage-stat">
                <div className="usage-stat-label">Projected total</div>
                <div className="usage-stat-value">
                  ~{formatBytes(usage.bytes_ingested + usage.metrics_forecast.projected_bytes_month)}/mo
                </div>
              </div>
            </div>
          )}
        </>
      )}

      {/* Plan comparison */}
      <div className="plan-grid">
        {PLANS.map(plan => {
          const isCurrent = plan.tier === currentTier
          return (
            <div
              key={plan.tier}
              className={`plan-tile${isCurrent ? ' plan-tile-current' : ''}`}
              data-testid={`account-plan-${plan.tier}`}
            >
              <div className="plan-tile-name">{plan.name}</div>
              <div className="plan-tile-price">
                <span className="plan-price-amount">${plan.price}</span>
                <span className="plan-price-period">/mo</span>
              </div>
              <div className="plan-tile-details">
                <div>{plan.volume}</div>
                <div>{plan.retention} retention</div>
                <div>{plan.overage} overage</div>
              </div>
              {isCurrent ? (
                <div className="plan-tile-badge">Current plan</div>
              ) : (
                <button
                  className="btn-action btn-action-primary plan-tile-btn"
                  onClick={() => handleUpgrade(plan.tier)}
                  disabled={upgrading !== null}
                  data-testid={`account-plan-select-${plan.tier}`}
                >
                  {upgrading === plan.tier ? 'Redirecting...' : isTrial ? 'Upgrade' : 'Switch'}
                </button>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}

// ── Integrations Card ──────────────────────────────────────────────────────

function IntegrationsCard({ integrations, onRefresh }: { integrations: IntegrationsResponse | null; onRefresh: () => void }) {
  const [pdName, setPdName] = useState('')
  const [pdKey, setPdKey] = useState('')
  const [pdSaving, setPdSaving] = useState(false)
  const [showPdForm, setShowPdForm] = useState(false)

  const slackConnected = integrations?.slack?.connected ?? false
  const pdKeys = integrations?.pagerduty?.keys ?? {}
  const pdConnected = Object.keys(pdKeys).length > 0

  async function handleSlackConnect() {
    try {
      const { url } = await connectSlack()
      window.location.href = url
    } catch {
      // Slack not configured on server
    }
  }

  async function handleSlackDisconnect() {
    await disconnectSlack()
    onRefresh()
  }

  async function handlePdSave(e: React.FormEvent) {
    e.preventDefault()
    if (!pdName.trim() || !pdKey.trim()) return
    setPdSaving(true)
    try {
      await savePagerDutyKey(pdName.trim(), pdKey.trim())
      setPdName('')
      setPdKey('')
      setShowPdForm(false)
      onRefresh()
    } finally {
      setPdSaving(false)
    }
  }

  async function handlePdDisconnect() {
    await disconnectPagerDuty()
    onRefresh()
  }

  return (
    <div className="dash-card" data-testid="account-integrations-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Integrations</h2>
      </div>

      {/* Slack */}
      <div className="integration-section">
        <div className="integration-row">
          <div className="integration-info">
            <span className="integration-name">Slack</span>
            {slackConnected ? (
              <span className="integration-status integration-status-connected">
                {integrations?.slack?.team_name || 'Connected'}
              </span>
            ) : (
              <span className="integration-status">Not connected</span>
            )}
          </div>
          {slackConnected ? (
            <button className="btn-action btn-action-danger" onClick={handleSlackDisconnect}
              data-testid="account-slack-disconnect-button">Disconnect</button>
          ) : (
            <button className="btn-action btn-action-primary" onClick={handleSlackConnect}
              data-testid="account-slack-connect-button">Connect Slack</button>
          )}
        </div>
      </div>

      {/* PagerDuty */}
      <div className="integration-section">
        <div className="integration-row">
          <div className="integration-info">
            <span className="integration-name">PagerDuty</span>
            {pdConnected ? (
              <span className="integration-status integration-status-connected">
                {Object.keys(pdKeys).length} service{Object.keys(pdKeys).length !== 1 ? 's' : ''}
              </span>
            ) : (
              <span className="integration-status">No keys configured</span>
            )}
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            {!showPdForm && (
              <button className="btn-action btn-action-primary" onClick={() => setShowPdForm(true)}
                data-testid="account-pd-add-button">+ Add key</button>
            )}
            {pdConnected && (
              <button className="btn-action btn-action-danger" onClick={handlePdDisconnect}
                data-testid="account-pd-disconnect-button">Clear all</button>
            )}
          </div>
        </div>

        {/* Existing PD keys */}
        {pdConnected && (
          <div className="integration-keys">
            {Object.entries(pdKeys).map(([name, masked]) => (
              <div key={name} className="integration-key-row">
                <span className="integration-key-name">{name}</span>
                <span className="integration-key-value">{masked}</span>
              </div>
            ))}
          </div>
        )}

        {/* Add PD key form */}
        {showPdForm && (
          <form className="create-token-form" onSubmit={handlePdSave} style={{ marginTop: 12 }}
            data-testid="account-pd-form">
            <div className="form-row">
              <div className="form-field">
                <label className="form-label">Service name</label>
                <input className="form-input" placeholder="api" value={pdName}
                  onChange={e => setPdName(e.target.value)} required
                  data-testid="account-pd-name-input" />
              </div>
              <div className="form-field">
                <label className="form-label">Routing key</label>
                <input className="form-input" placeholder="paste from PagerDuty" value={pdKey}
                  onChange={e => setPdKey(e.target.value)} required
                  data-testid="account-pd-key-input" />
              </div>
            </div>
            <div className="form-actions">
              <button type="submit" className="btn-action btn-action-primary" disabled={pdSaving}
                data-testid="account-pd-save-button">{pdSaving ? 'Saving...' : 'Save'}</button>
              <button type="button" className="btn-action" onClick={() => setShowPdForm(false)}>Cancel</button>
            </div>
          </form>
        )}
      </div>

      {/* Info */}
      <div className="dash-empty" style={{ paddingTop: 12 }}>
        Configure notification routing in <code>/etc/notify.yaml</code> or per-cron with <code># notify: slack:#channel</code>
      </div>
      <div className="dash-empty" data-testid="account-integrations-cli-hint">
        Setup via CLI: <code>weft integration slack connect</code>,{' '}
        <code>weft integration pagerduty add &lt;name&gt; --key &lt;key&gt;</code>, <code>weft integration list</code>.
      </div>
    </div>
  )
}

// ── Team Members Card ──────────────────────────────────────────────────────

function TeamCard({ team, onRefresh, currentEmail }: { team: TeamResponse | null; onRefresh: () => void; currentEmail: string }) {
  const [showInvite, setShowInvite] = useState(false)
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviting, setInviting] = useState(false)
  const [inviteError, setInviteError] = useState('')
  const [removing, setRemoving] = useState<string | null>(null)
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null)
  const [editingName, setEditingName] = useState(false)
  const [orgNameInput, setOrgNameInput] = useState(team?.org_name ?? '')

  const members = team?.members ?? []
  const seatsUsed = team?.seats_used ?? 0
  const seatsLimit = team?.seats_limit ?? 1
  const atLimit = seatsUsed >= seatsLimit
  const isOwner = members.some(m => m.Email === currentEmail && m.Role === 'admin')

  async function handleInvite(e: React.FormEvent) {
    e.preventDefault()
    if (!inviteEmail.trim()) return
    setInviteError('')
    setInviting(true)
    try {
      await inviteMember(inviteEmail.trim())
      setInviteEmail('')
      setShowInvite(false)
      onRefresh()
    } catch (err) {
      setInviteError(err instanceof Error ? err.message : 'Failed to invite.')
    } finally {
      setInviting(false)
    }
  }

  async function handleRemove(id: string) {
    setRemoving(id)
    try {
      await removeMember(id)
      setConfirmRemove(null)
      onRefresh()
    } catch {
      // ignore
    } finally {
      setRemoving(null)
    }
  }

  return (
    <div className="dash-card" data-testid="account-team-card">
      <div className="dash-card-header">
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 10 }}>
          <h2 className="dash-card-title">Team</h2>
          {editingName ? (
            <form onSubmit={async (e) => {
              e.preventDefault()
              if (orgNameInput.trim()) {
                await updateTeamName(orgNameInput.trim())
                onRefresh()
              }
              setEditingName(false)
            }} style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <input
                className="form-input"
                value={orgNameInput}
                onChange={e => setOrgNameInput(e.target.value)}
                autoFocus
                style={{ width: 180, padding: '4px 8px', fontSize: '0.82rem' }}
                data-testid="account-org-name-input"
              />
              <button type="submit" className="btn-action btn-action-primary" style={{ padding: '4px 10px', fontSize: '0.75rem' }}>Save</button>
              <button type="button" className="btn-action" style={{ padding: '4px 10px', fontSize: '0.75rem' }} onClick={() => setEditingName(false)}>Cancel</button>
            </form>
          ) : (
            <span
              className="watch-tile-name"
              style={{ cursor: isOwner ? 'pointer' : 'default' }}
              onClick={() => { if (isOwner) { setOrgNameInput(team?.org_name ?? ''); setEditingName(true) } }}
              title={isOwner ? 'Click to rename' : undefined}
              data-testid="account-org-name"
            >
              {team?.org_name || '—'}
            </span>
          )}
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span className="watch-count-badge">{seatsUsed} / {seatsLimit} seats</span>
          {isOwner && !showInvite && (
            <button
              className="btn-action btn-action-primary"
              onClick={() => setShowInvite(true)}
              disabled={atLimit}
              title={atLimit ? 'Seat limit reached — upgrade to add more' : undefined}
              data-testid="account-invite-button"
            >
              + Invite
            </button>
          )}
        </div>
      </div>

      {showInvite && (
        <form className="create-token-form" onSubmit={handleInvite} data-testid="account-invite-form">
          <div className="form-row">
            <div className="form-field">
              <label className="form-label">Email address</label>
              <input
                className="form-input"
                type="email"
                placeholder="teammate@company.com"
                value={inviteEmail}
                onChange={e => setInviteEmail(e.target.value)}
                autoFocus
                required
                data-testid="account-invite-email-input"
              />
            </div>
          </div>
          {inviteError && <div className="form-error">{inviteError}</div>}
          <div className="form-actions">
            <button
              type="submit"
              className="btn-action btn-action-primary"
              disabled={inviting || !inviteEmail.trim()}
              data-testid="account-invite-submit-button"
            >
              {inviting ? 'Sending...' : 'Send invite'}
            </button>
            <button
              type="button"
              className="btn-action"
              onClick={() => { setShowInvite(false); setInviteError('') }}
              data-testid="account-invite-cancel-button"
            >
              Cancel
            </button>
          </div>
        </form>
      )}

      {members.length === 0 ? (
        <div className="dash-empty">No team members yet.</div>
      ) : (
        <div className="dash-table-wrap">
          <table className="dash-table" data-testid="account-team-table">
            <thead>
              <tr><th>Email</th><th>Role</th><th>Status</th><th>Joined</th><th></th></tr>
            </thead>
            <tbody>
              {members.map(m => (
                <tr key={m.ID} data-testid={`account-team-row-${m.ID}`}>
                  <td className="token-label">{m.Email}</td>
                  <td>
                    <span className={`token-status ${m.Role === 'admin' ? 'token-status-active' : ''}`}>
                      {m.Role}
                    </span>
                  </td>
                  <td>
                    <span className={`token-status ${m.Status === 'active' ? 'token-status-active' : 'token-status-revoked'}`}>
                      {m.Status === 'pending' ? 'invited' : m.Status}
                    </span>
                  </td>
                  <td className="token-date">
                    {m.AcceptedAt ? formatDate(m.AcceptedAt) : m.Status === 'pending' ? formatDate(m.InvitedAt) : '—'}
                  </td>
                  <td>
                    {isOwner && m.Role !== 'admin' && (
                      confirmRemove === m.ID ? (
                        <div style={{ display: 'flex', gap: 6 }}>
                          <button
                            className="btn-action btn-action-danger"
                            onClick={() => handleRemove(m.ID)}
                            disabled={removing === m.ID}
                            data-testid={`account-team-confirm-remove-${m.ID}`}
                          >
                            {removing === m.ID ? '...' : 'Confirm'}
                          </button>
                          <button
                            className="btn-action"
                            onClick={() => setConfirmRemove(null)}
                          >
                            Cancel
                          </button>
                        </div>
                      ) : (
                        <button
                          className="btn-action btn-action-danger"
                          onClick={() => setConfirmRemove(m.ID)}
                          data-testid={`account-team-remove-${m.ID}`}
                        >
                          Remove
                        </button>
                      )
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

// ── Permissions Card ───────────────────────────────────────────────────────

// PermissionsCard is the owner-administered grid that grants each
// non-owner member the manager groups (edit access to /bin, /cron.d,
// /etc) and per-namespace read / token-mint access. It's the browser
// twin of the shell's adduser/deluser; both write the same /api/
// permissions document. The namespace cell expands into per-namespace
// read + tokens toggles.
function PermissionsCard({ team, isOwner, currentEmail }: { team: TeamResponse | null; isOwner: boolean; currentEmail: string }) {
  const [doc, setDoc] = useState<PermissionsDoc | null>(null)
  const [namespaces, setNamespaces] = useState<NamespaceInfo[]>([])
  const [expanded, setExpanded] = useState<string | null>(null)
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState('')

  useEffect(() => {
    getPermissions().then(setDoc).catch(() => setDoc({}))
    getNamespaces().then(setNamespaces).catch(() => setNamespaces([]))
  }, [])

  if (!team || !doc) return null
  const nonOwners = (team.members ?? []).filter(m => m.Role !== 'admin' && m.Status === 'active' && m.UserID)
  if (nonOwners.length === 0) return null

  function permsFor(email: string): MemberPerms {
    return doc?.[email] ?? { bin: 'read-only', cron: 'read-only', etc: 'read-only' }
  }

  function flip(email: string, key: 'bin' | 'cron' | 'etc') {
    if (!doc) return
    const cur = permsFor(email)
    const next: MemberPerms = { ...cur, [key]: cur[key] === 'edit' ? 'read-only' : 'edit' }
    setDoc({ ...doc, [email]: next })
    setDirty(true)
  }

  // flipNs toggles one facet (read or create_tokens) of a member's
  // namespace grant. Read and tokens are independent — matching the
  // shell's `<ns>` vs `<ns>+tokens` groups. A namespace with neither
  // facet set is dropped so it doesn't linger as an empty grant.
  function flipNs(email: string, ns: string, facet: 'read' | 'create_tokens') {
    if (!doc) return
    const cur = permsFor(email)
    const nsMap = { ...(cur.namespaces ?? {}) }
    const acl = { ...(nsMap[ns] ?? { read: false }) }
    acl[facet] = !acl[facet]
    if (!acl.read && !acl.create_tokens) delete nsMap[ns]
    else nsMap[ns] = acl
    setDoc({ ...doc, [email]: { ...cur, namespaces: nsMap } })
    setDirty(true)
  }

  async function handleSave() {
    if (!doc) return
    setSaving(true)
    setSaveError('')
    try {
      await setPermissions(doc)
      setDirty(false)
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : 'Failed to save permissions.')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="dash-card" data-testid="account-permissions-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Permissions</h2>
        {isOwner && (
          <button
            className="btn-action btn-action-primary"
            onClick={handleSave}
            disabled={!dirty || saving}
            data-testid="account-permissions-save-button"
          >
            {saving ? 'Saving...' : 'Save changes'}
          </button>
        )}
      </div>
      {!isOwner && (
        <div className="dash-empty" style={{ marginBottom: 12 }}>
          Read-only view. Owners can edit write access to <code>/bin</code>, <code>/cron.d</code>, <code>/etc</code> and per-namespace access.
        </div>
      )}
      {saveError && <div className="form-error">{saveError}</div>}
      <div className="dash-table-wrap">
        <table className="dash-table" data-testid="account-permissions-table">
          <thead>
            <tr>
              <th>Email</th>
              <th>/bin</th>
              <th>/cron.d</th>
              <th>/etc</th>
              <th>Namespaces</th>
            </tr>
          </thead>
          <tbody>
            {nonOwners.map(m => {
              const p = permsFor(m.Email)
              const nsSummary = summarizeNs(p)
              const isExpanded = expanded === m.Email
              return (
                <Fragment key={m.ID}>
                  <tr data-testid={`account-permissions-row-${m.ID}`}>
                    <td className="token-label">{m.Email}{m.Email === currentEmail && ' (you)'}</td>
                    <PermCheckCell isOwner={isOwner} level={p.bin} testIdBase={`account-permissions-${m.ID}-bin`} onFlip={() => flip(m.Email, 'bin')} />
                    <PermCheckCell isOwner={isOwner} level={p.cron} testIdBase={`account-permissions-${m.ID}-cron`} onFlip={() => flip(m.Email, 'cron')} />
                    <PermCheckCell isOwner={isOwner} level={p.etc} testIdBase={`account-permissions-${m.ID}-etc`} onFlip={() => flip(m.Email, 'etc')} />
                    <td>
                      <button
                        className="btn-action"
                        onClick={() => setExpanded(isExpanded ? null : m.Email)}
                        title={nsSummary.title}
                        data-testid={`account-permissions-${m.ID}-ns-toggle`}
                      >
                        {nsSummary.short} {isExpanded ? '▾' : '▸'}
                      </button>
                    </td>
                  </tr>
                  {isExpanded && (
                    <tr data-testid={`account-permissions-${m.ID}-ns-panel`}>
                      <td colSpan={5} style={{ background: 'rgba(255,255,255,0.02)' }}>
                        <NamespaceGrants
                          memberId={m.ID}
                          isOwner={isOwner}
                          namespaces={namespaces}
                          perms={p}
                          onFlip={(ns, facet) => flipNs(m.Email, ns, facet)}
                        />
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
      <div className="dash-empty" style={{ marginTop: 10, fontSize: '0.78rem' }}>
        The same grants are available in the shell: <code>adduser &lt;email&gt; &lt;namespace&gt;</code> / <code>&lt;namespace&gt;+tokens</code>. See <code>man passwd</code>.
      </div>
    </div>
  )
}

// NamespaceGrants is the expanded per-member panel: one row per
// namespace with independent read and token-mint checkboxes.
function NamespaceGrants({ memberId, isOwner, namespaces, perms, onFlip }: {
  memberId: string
  isOwner: boolean
  namespaces: NamespaceInfo[]
  perms: MemberPerms
  onFlip: (ns: string, facet: 'read' | 'create_tokens') => void
}) {
  if (namespaces.length === 0) {
    return <div className="dash-empty" style={{ padding: '8px 4px' }}>No namespaces yet.</div>
  }
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'minmax(120px,1fr) auto auto', gap: '4px 16px', alignItems: 'center', padding: '8px 4px' }}>
      <div style={{ fontWeight: 600, fontSize: '0.78rem', opacity: 0.7 }}>namespace</div>
      <div style={{ fontWeight: 600, fontSize: '0.78rem', opacity: 0.7 }}>read</div>
      <div style={{ fontWeight: 600, fontSize: '0.78rem', opacity: 0.7 }}>+tokens</div>
      {namespaces.map(ns => {
        const acl = perms.namespaces?.[ns.name] ?? { read: false }
        return (
          <Fragment key={ns.name}>
            <div className="token-label" style={{ fontSize: '0.85rem' }}>{ns.name}</div>
            <input
              type="checkbox"
              checked={!!acl.read}
              disabled={!isOwner}
              onChange={() => onFlip(ns.name, 'read')}
              data-testid={`account-permissions-${memberId}-ns-${ns.name}-read-checkbox`}
              aria-label={`${ns.name} read`}
            />
            <input
              type="checkbox"
              checked={!!acl.create_tokens}
              disabled={!isOwner}
              onChange={() => onFlip(ns.name, 'create_tokens')}
              data-testid={`account-permissions-${memberId}-ns-${ns.name}-tokens-checkbox`}
              aria-label={`${ns.name} create tokens`}
            />
          </Fragment>
        )
      })}
    </div>
  )
}

function PermCheckCell({ isOwner, level, testIdBase, onFlip }: { isOwner: boolean; level: AccessLevel; testIdBase: string; onFlip: () => void }) {
  const editable = isOwner
  return (
    <td>
      <input
        type="checkbox"
        checked={level === 'edit'}
        disabled={!editable}
        onChange={onFlip}
        data-testid={`${testIdBase}-checkbox`}
        aria-label={level === 'edit' ? 'editable' : 'read-only'}
      />
    </td>
  )
}

function summarizeNs(p: MemberPerms): { short: string; title: string } {
  const granted = Object.entries(p.namespaces ?? {}).filter(([, acl]) => acl.read || acl.create_tokens)
  if (granted.length === 0) return { short: 'none', title: 'No namespace access' }
  const labels = granted.map(([n, acl]) => acl.create_tokens ? `${n} (+tokens)` : n)
  return { short: `${granted.length} ns`, title: labels.join(', ') }
}

// ── Main ───────────────────────────────────────────────────────────────────

export default function Account() {
  const { user } = useAuth()
  const [usage, setUsage] = useState<Usage | null>(null)
  const [team, setTeam] = useState<TeamResponse | null>(null)
  const [integrations, setIntegrations] = useState<IntegrationsResponse | null>(null)

  function loadData() {
    getUsage().then(setUsage).catch(() => {})
    getTeam().then(setTeam).catch(() => {})
    getIntegrations().then(setIntegrations).catch(() => {})
  }

  useEffect(() => { loadData() }, [])

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="account-main">
        <div className="dash-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Account</h1>
            {user && <div className="dash-empty" style={{ marginTop: 4 }}>{user.email}</div>}
          </div>

          <SubscriptionCard usage={usage} onRefresh={loadData} />
          <IntegrationsCard integrations={integrations} onRefresh={loadData} />
          <TeamCard team={team} onRefresh={loadData} currentEmail={user?.email ?? ''} />
          <PermissionsCard
            team={team}
            isOwner={(team?.members ?? []).some(m => m.Email === user?.email && m.Role === 'admin')}
            currentEmail={user?.email ?? ''}
          />
        </div>
      </main>
    </div>
  )
}
