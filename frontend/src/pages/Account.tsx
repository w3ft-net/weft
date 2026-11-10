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
import { PLANS, DEMO_URL } from '../lib/plans'
import './Dashboard.css'

const ELEMENTS_APPEARANCE = {
  theme: 'night' as const,
  variables: {
    // Mirrors the TUI tokens in index.css (Stripe can't read CSS vars).
    colorPrimary: '#5fafd7',
    colorBackground: '#0f0f1a',
    colorText: '#c0c0d0',
    colorDanger: '#ff5f5f',
    fontFamily: "'JetBrains Mono', ui-monospace, monospace",
    borderRadius: '0px',
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

// Meter renders a usage gauge on the character grid:
//   [████████████░░░░░░░░░░░░░░░░] 38%
// `cls` is the status colour class (usage-ok / -warn / -crit).
const METER_CELLS = 40

function Meter({ pct, cls, testId }: { pct: number; cls: string; testId?: string }) {
  const filled = Math.round((pct / 100) * METER_CELLS)
  return (
    <div className="usage-bar-wrap">
      <span className="usage-meter" data-testid={testId} role="meter" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
        <span className="usage-meter-bracket">[</span>
        <span className={cls}>{'█'.repeat(filled)}</span>
        <span className="usage-meter-empty">{'░'.repeat(METER_CELLS - filled)}</span>
        <span className="usage-meter-bracket">]</span>
      </span>
      <span className={`usage-bar-pct ${cls}`}>{pct}%</span>
    </div>
  )
}

// ── Plan tiers ─────────────────────────────────────────────────────────────


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
      {error && <div className="form-error" data-testid="account-confirm-error">{error}</div>}
      <div className="form-actions account-confirm-actions">
        <button
          type="submit"
          className="btn-action btn-action-primary"
          disabled={!stripe || submitting}
          data-testid="account-confirm-submit-button"
        >
          {submitting ? 'confirming...' : 'confirm payment'}
        </button>
        <button
          type="button"
          className="btn-action"
          onClick={() => onDone('cancelled')}
          data-testid="account-confirm-cancel-button"
        >
          cancel
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
  const meterClass = pct > 90 ? 'usage-crit' : pct > 70 ? 'usage-warn' : 'usage-ok'

  return (
    <div className="dash-card" data-testid="account-subscription-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">subscription</h2>
        {usage && <span className="dash-tier-badge" data-testid="account-tier-badge">{usage.tier}</span>}
      </div>

      {/* Trial banner */}
      {isTrial && (
        <div className="trial-banner" data-testid="account-trial-banner">
          You're on a free trial. Choose a plan below to continue after the trial ends.
        </div>
      )}

      {error && (
        <div className="form-error" data-testid="account-subscription-error">
          {error}
        </div>
      )}

      {confirm && (
        <div className="dash-card dash-card-accent account-confirm" data-testid="account-confirm-modal">
          <div className="dash-card-header">
            <h3 className="dash-card-title">confirm payment</h3>
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
              <div className="usage-stat-label">ingested this period</div>
              <div className="usage-stat-value usage-stat-value-hi" data-testid="account-bytes-ingested">
                {formatBytes(usage.bytes_ingested)}
              </div>
            </div>
            <div className="usage-stat">
              <div className="usage-stat-label">tier limit</div>
              <div className="usage-stat-value">{formatBytes(usage.tier_limit_bytes)}</div>
            </div>
            <div className="usage-stat">
              <div className="usage-stat-label">period start</div>
              <div className="usage-stat-value">{formatDate(usage.period_start)}</div>
            </div>
          </div>
          <Meter pct={pct} cls={meterClass} testId="account-usage-bar" />
          {usage.projected_overage_cents > 0 && (
            <div
              className="usage-projected-overage"
              data-testid="account-projected-overage"
            >
              projected overage this period:{' '}
              <strong>${(usage.projected_overage_cents / 100).toFixed(2)}</strong>
              <span className="usage-projected-overage-hint">
                {' '}— billed at period end.
              </span>
            </div>
          )}

          {/* Metrics forecast */}
          {usage.metrics_forecast && usage.metrics_forecast.agents_with_metrics > 0 && (
            <div className="usage-stats" data-testid="account-metrics-forecast">
              <div className="usage-stat">
                <div className="usage-stat-label">metrics agents</div>
                <div className="usage-stat-value">{usage.metrics_forecast.agents_with_metrics}</div>
              </div>
              <div className="usage-stat">
                <div className="usage-stat-label">projected metrics</div>
                <div className="usage-stat-value">~{formatBytes(usage.metrics_forecast.projected_bytes_month)}/mo</div>
              </div>
              <div className="usage-stat">
                <div className="usage-stat-label">projected total</div>
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
                {plan.price === null ? (
                  <span className="plan-price-amount">custom</span>
                ) : (
                  <>
                    <span className="plan-price-amount">${plan.price}</span>
                    <span className="plan-price-period">/mo</span>
                  </>
                )}
              </div>
              <div className="plan-tile-details">
                {plan.blurb ?? (
                  <>
                    <div>{plan.volume}</div>
                    <div>{plan.retention} retention</div>
                    <div>{plan.overage} overage</div>
                  </>
                )}
              </div>
              {isCurrent ? (
                <div className="plan-tile-badge">current plan</div>
              ) : plan.price === null ? (
                <a
                  className="btn-action btn-action-primary plan-tile-btn"
                  href={DEMO_URL}
                  target="_blank"
                  rel="noopener noreferrer"
                  data-testid={`account-plan-select-${plan.tier}`}
                >
                  talk to us
                </a>
              ) : (
                <button
                  className="btn-action btn-action-primary plan-tile-btn"
                  onClick={() => handleUpgrade(plan.tier)}
                  disabled={upgrading !== null}
                  data-testid={`account-plan-select-${plan.tier}`}
                >
                  {upgrading === plan.tier ? 'redirecting...' : isTrial ? 'upgrade' : 'switch'}
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
        <h2 className="dash-card-title">integrations</h2>
      </div>

      {/* Slack */}
      <div className="integration-section">
        <div className="integration-row">
          <div className="integration-info">
            <span className="integration-name">Slack</span>
            {slackConnected ? (
              <span className="integration-status integration-status-connected">
                {integrations?.slack?.team_name || 'connected'}
              </span>
            ) : (
              <span className="integration-status">not connected</span>
            )}
          </div>
          {slackConnected ? (
            <button className="btn-action btn-action-danger" onClick={handleSlackDisconnect}
              data-testid="account-slack-disconnect-button">disconnect</button>
          ) : (
            <button className="btn-action btn-action-primary" onClick={handleSlackConnect}
              data-testid="account-slack-connect-button">connect slack</button>
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
              <span className="integration-status">no keys configured</span>
            )}
          </div>
          <div className="integration-actions">
            {!showPdForm && (
              <button className="btn-action btn-action-primary" onClick={() => setShowPdForm(true)}
                data-testid="account-pd-add-button">+ add key</button>
            )}
            {pdConnected && (
              <button className="btn-action btn-action-danger" onClick={handlePdDisconnect}
                data-testid="account-pd-disconnect-button">clear all</button>
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
          <form className="create-token-form" onSubmit={handlePdSave}
            data-testid="account-pd-form">
            <div className="form-row">
              <div className="form-field">
                <label className="form-label">service name</label>
                <input className="form-input" placeholder="api" value={pdName}
                  onChange={e => setPdName(e.target.value)} required
                  data-testid="account-pd-name-input" />
              </div>
              <div className="form-field">
                <label className="form-label">routing key</label>
                <input className="form-input" placeholder="paste from PagerDuty" value={pdKey}
                  onChange={e => setPdKey(e.target.value)} required
                  data-testid="account-pd-key-input" />
              </div>
            </div>
            <div className="form-actions">
              <button type="submit" className="btn-action btn-action-primary" disabled={pdSaving}
                data-testid="account-pd-save-button">{pdSaving ? 'saving...' : 'save'}</button>
              <button type="button" className="btn-action" onClick={() => setShowPdForm(false)}
                data-testid="account-pd-cancel-button">cancel</button>
            </div>
          </form>
        )}
      </div>

      {/* Info */}
      <div className="dash-empty">
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

export function TeamCard({ team, onRefresh, currentEmail }: { team: TeamResponse | null; onRefresh: () => void; currentEmail: string }) {
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
  const isOwner = members.some(m => m.email === currentEmail && m.role === 'admin')

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
        <div className="dash-card-header-group">
          <h2 className="dash-card-title">team</h2>
          {editingName ? (
            <form onSubmit={async (e) => {
              e.preventDefault()
              if (orgNameInput.trim()) {
                await updateTeamName(orgNameInput.trim())
                onRefresh()
              }
              setEditingName(false)
            }} className="form-actions">
              <input
                value={orgNameInput}
                onChange={e => setOrgNameInput(e.target.value)}
                autoFocus
                className="form-input form-input-inline"
                data-testid="account-org-name-input"
              />
              <button type="submit" className="btn-action btn-action-primary" data-testid="account-org-name-save-button">save</button>
              <button type="button" className="btn-action" onClick={() => setEditingName(false)} data-testid="account-org-name-cancel-button">cancel</button>
            </form>
          ) : (
            <span
              className={`watch-tile-name${isOwner ? ' watch-tile-name-editable' : ''}`}
              onClick={() => { if (isOwner) { setOrgNameInput(team?.org_name ?? ''); setEditingName(true) } }}
              title={isOwner ? 'Click to rename' : undefined}
              data-testid="account-org-name"
            >
              {team?.org_name || '—'}
            </span>
          )}
        </div>
        <div className="dash-card-header-group">
          <span className={`watch-count-badge${atLimit ? ' watch-count-at-limit' : ''}`}>{seatsUsed}/{seatsLimit} seats</span>
          {isOwner && !showInvite && (
            <button
              className="btn-action btn-action-primary"
              onClick={() => setShowInvite(true)}
              disabled={atLimit}
              title={atLimit ? 'Seat limit reached — upgrade to add more' : undefined}
              data-testid="account-invite-button"
            >
              + invite
            </button>
          )}
        </div>
      </div>

      {showInvite && (
        <form className="create-token-form" onSubmit={handleInvite} data-testid="account-invite-form">
          <div className="form-row">
            <div className="form-field">
              <label className="form-label">email address</label>
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
              {inviting ? 'sending...' : 'send invite'}
            </button>
            <button
              type="button"
              className="btn-action"
              onClick={() => { setShowInvite(false); setInviteError('') }}
              data-testid="account-invite-cancel-button"
            >
              cancel
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
              <tr><th>email</th><th>role</th><th>status</th><th>joined</th><th></th></tr>
            </thead>
            <tbody>
              {members.map(m => (
                <tr key={m.id} data-testid={`account-team-row-${m.id}`}>
                  <td className="token-label">{m.email}</td>
                  <td>
                    <span className={`token-status ${m.role === 'admin' ? 'token-status-active' : ''}`}>
                      {m.role}
                    </span>
                  </td>
                  <td>
                    <span className={`token-status ${m.status === 'active' ? 'token-status-active' : 'token-status-revoked'}`}>
                      {m.status === 'pending' ? 'invited' : m.status}
                    </span>
                  </td>
                  <td className="token-date">
                    {m.acceptedAt ? formatDate(m.acceptedAt) : m.status === 'pending' && m.invitedAt ? formatDate(m.invitedAt) : '—'}
                  </td>
                  <td>
                    {isOwner && m.role !== 'admin' && (
                      confirmRemove === m.id ? (
                        <div className="form-actions">
                          <button
                            className="btn-action btn-action-danger"
                            onClick={() => handleRemove(m.id)}
                            disabled={removing === m.id}
                            data-testid={`account-team-confirm-remove-${m.id}`}
                          >
                            {removing === m.id ? '...' : 'confirm'}
                          </button>
                          <button
                            className="btn-action"
                            onClick={() => setConfirmRemove(null)}
                            data-testid={`account-team-cancel-remove-${m.id}`}
                          >
                            cancel
                          </button>
                        </div>
                      ) : (
                        <button
                          className="btn-action btn-action-danger"
                          onClick={() => setConfirmRemove(m.id)}
                          data-testid={`account-team-remove-${m.id}`}
                        >
                          remove
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
// read + write toggles.
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
  const nonOwners = (team.members ?? []).filter(m => m.role !== 'admin' && m.status === 'active' && m.userId)
  if (nonOwners.length === 0) return null

  function permsFor(email: string): MemberPerms {
    return doc?.[email] ?? { etc: 'read-only' }
  }

  function flip(email: string, key: 'etc') {
    if (!doc) return
    const cur = permsFor(email)
    const next: MemberPerms = { ...cur, [key]: cur[key] === 'edit' ? 'read-only' : 'edit' }
    setDoc({ ...doc, [email]: next })
    setDirty(true)
  }

  // flipNs toggles one facet (read or write) of a member's namespace grant,
  // matching the shell's `<ns>-r` and `<ns>-w` groups. A namespace with
  // neither facet set is dropped so it doesn't linger as an empty grant.
  function flipNs(email: string, ns: string, facet: 'read' | 'write') {
    if (!doc) return
    const cur = permsFor(email)
    const nsMap = { ...(cur.namespaces ?? {}) }
    const acl = { ...(nsMap[ns] ?? { read: false }) }
    acl[facet] = !acl[facet]
    if (!acl.read && !acl.write) delete nsMap[ns]
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
        <h2 className="dash-card-title">permissions</h2>
        {isOwner && (
          <button
            className="btn-action btn-action-primary"
            onClick={handleSave}
            disabled={!dirty || saving}
            data-testid="account-permissions-save-button"
          >
            {saving ? 'saving...' : 'save changes'}
          </button>
        )}
      </div>
      {!isOwner && (
        <div className="dash-empty">
          Read-only view. Owners can edit access to <code>/etc</code> and per-namespace read and write access.
        </div>
      )}
      {saveError && <div className="form-error">{saveError}</div>}
      <div className="dash-table-wrap">
        <table className="dash-table" data-testid="account-permissions-table">
          <thead>
            <tr>
              <th>email</th>
              <th>/etc</th>
              <th>namespaces</th>
            </tr>
          </thead>
          <tbody>
            {nonOwners.map(m => {
              const p = permsFor(m.email)
              const nsSummary = summarizeNs(p)
              const isExpanded = expanded === m.email
              return (
                <Fragment key={m.id}>
                  <tr data-testid={`account-permissions-row-${m.id}`}>
                    <td className="token-label">{m.email}{m.email === currentEmail && ' (you)'}</td>
                    <PermCheckCell isOwner={isOwner} level={p.etc} testIdBase={`account-permissions-${m.id}-etc`} onFlip={() => flip(m.email, 'etc')} />
                    <td>
                      <button
                        className="btn-action"
                        onClick={() => setExpanded(isExpanded ? null : m.email)}
                        title={nsSummary.title}
                        data-testid={`account-permissions-${m.id}-ns-toggle`}
                      >
                        {nsSummary.short} {isExpanded ? '▾' : '▸'}
                      </button>
                    </td>
                  </tr>
                  {isExpanded && (
                    <tr className="dash-table-subrow" data-testid={`account-permissions-${m.id}-ns-panel`}>
                      <td colSpan={3}>
                        <NamespaceGrants
                          memberId={m.id}
                          isOwner={isOwner}
                          namespaces={namespaces}
                          perms={p}
                          onFlip={(ns, facet) => flipNs(m.email, ns, facet)}
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
      <div className="dash-empty dash-hint">
        The same grants are available in the shell: <code>usermod -aG &lt;namespace&gt;-r|-w &lt;email&gt;</code>. See <code>man passwd</code>.
      </div>
    </div>
  )
}

// NamespaceGrants is the expanded per-member panel: one row per
// namespace with independent read and write checkboxes.
function NamespaceGrants({ memberId, isOwner, namespaces, perms, onFlip }: {
  memberId: string
  isOwner: boolean
  namespaces: NamespaceInfo[]
  perms: MemberPerms
  onFlip: (ns: string, facet: 'read' | 'write') => void
}) {
  if (namespaces.length === 0) {
    return <div className="dash-empty">No namespaces yet.</div>
  }
  return (
    <div className="ns-grants">
      <div className="ns-grants-head">namespace</div>
      <div className="ns-grants-head">read</div>
      <div className="ns-grants-head">write</div>
      {namespaces.map(ns => {
        const acl = perms.namespaces?.[ns.name] ?? { read: false }
        return (
          <Fragment key={ns.name}>
            <div className="token-label">{ns.name}</div>
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
              checked={!!acl.write}
              disabled={!isOwner}
              onChange={() => onFlip(ns.name, 'write')}
              data-testid={`account-permissions-${memberId}-ns-${ns.name}-write-checkbox`}
              aria-label={`${ns.name} write`}
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
  const granted = Object.entries(p.namespaces ?? {}).filter(([, acl]) => acl.read || acl.write)
  if (granted.length === 0) return { short: 'none', title: 'No namespace access' }
  const labels = granted.map(([n, acl]) => acl.write ? `${n} (write)` : n)
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
            {user && <div className="dash-empty">{user.email}</div>}
          </div>

          <SubscriptionCard usage={usage} onRefresh={loadData} />
          <IntegrationsCard integrations={integrations} onRefresh={loadData} />
          <TeamCard team={team} onRefresh={loadData} currentEmail={user?.email ?? ''} />
          <PermissionsCard
            team={team}
            isOwner={(team?.members ?? []).some(m => m.email === user?.email && m.role === 'admin')}
            currentEmail={user?.email ?? ''}
          />
        </div>
      </main>
    </div>
  )
}
