import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  getUsage, getTokens, createToken, revokeToken,
  getQueries, deleteQuery,
  type Usage, type AgentToken, type SavedQuery,
} from '../lib/api'
import './Dashboard.css'

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

function truncateToken(tok: string): string {
  if (tok.length <= 16) return tok
  return `${tok.slice(0, 8)}...${tok.slice(-4)}`
}

// ── Usage Card ──────────────────────────────────────────────────────────────

function UsageCard({ usage }: { usage: Usage }) {
  const pct = Math.min(100, Math.round((usage.bytes_ingested / usage.tier_limit_bytes) * 100))
  const barColor = pct > 90 ? 'var(--red)' : pct > 70 ? 'var(--yellow)' : 'var(--green)'

  return (
    <div className="dash-card" data-testid="dashboard-usage-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Usage</h2>
        <span className="dash-tier-badge" data-testid="dashboard-tier-badge">{usage.tier}</span>
      </div>
      <div className="usage-stats">
        <div className="usage-stat">
          <div className="usage-stat-label">Ingested this period</div>
          <div className="usage-stat-value" data-testid="dashboard-bytes-ingested">
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
      <div className="usage-bar-wrap">
        <div className="usage-bar-track" data-testid="dashboard-usage-bar">
          <div
            className="usage-bar-fill"
            style={{ width: `${pct}%`, background: barColor }}
          />
        </div>
        <span className="usage-bar-pct" style={{ color: barColor }}>{pct}%</span>
      </div>
    </div>
  )
}

// ── Tokens Section ──────────────────────────────────────────────────────────

interface CreateTokenFormProps {
  onCreated: (t: AgentToken & { secret: string }) => void
  onCancel: () => void
}

function CreateTokenForm({ onCreated, onCancel }: CreateTokenFormProps) {
  const [label, setLabel] = useState('')
  const [app, setApp] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!label.trim()) return
    setError('')
    setLoading(true)
    try {
      const tok = await createToken(label.trim(), app.trim())
      onCreated(tok)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create token.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <form className="create-token-form" onSubmit={handleSubmit} data-testid="dashboard-create-token-form">
      <div className="form-row">
        <div className="form-field">
          <label className="form-label">Label</label>
          <input
            className="form-input"
            placeholder="prod-web-01"
            value={label}
            onChange={e => setLabel(e.target.value)}
            data-testid="dashboard-token-label-input"
            required
          />
        </div>
        <div className="form-field">
          <label className="form-label">App</label>
          <input
            className="form-input"
            placeholder="myapp"
            value={app}
            onChange={e => setApp(e.target.value)}
            data-testid="dashboard-token-app-input"
          />
        </div>
      </div>
      {error && <div className="form-error">{error}</div>}
      <div className="form-actions">
        <button
          type="submit"
          className="btn-action btn-action-primary"
          disabled={loading}
          data-testid="dashboard-token-create-submit-button"
        >
          {loading ? 'Creating...' : 'Create token'}
        </button>
        <button
          type="button"
          className="btn-action"
          onClick={onCancel}
          data-testid="dashboard-token-create-cancel-button"
        >
          Cancel
        </button>
      </div>
    </form>
  )
}

interface NewTokenRevealProps {
  token: AgentToken & { secret: string }
  onDone: () => void
}

function NewTokenReveal({ token, onDone }: NewTokenRevealProps) {
  const [copied, setCopied] = useState(false)

  function handleCopy() {
    navigator.clipboard.writeText(token.secret)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="token-reveal" data-testid="dashboard-token-reveal">
      <div className="token-reveal-label">
        Token created — copy now, it won't be shown again.
      </div>
      <div className="token-reveal-value" data-testid="dashboard-token-secret">
        {token.secret}
      </div>
      <div className="form-actions">
        <button
          className="btn-action btn-action-primary"
          onClick={handleCopy}
          data-testid="dashboard-token-copy-button"
        >
          {copied ? 'Copied!' : 'Copy to clipboard'}
        </button>
        <button
          className="btn-action"
          onClick={onDone}
          data-testid="dashboard-token-reveal-done-button"
        >
          Done
        </button>
      </div>
    </div>
  )
}

interface TokensCardProps {
  tokens: AgentToken[]
  onRevoke: (tok: string) => void
}

function TokensCard({ tokens, onRevoke }: TokensCardProps) {
  const [showForm, setShowForm] = useState(false)
  const [newToken, setNewToken] = useState<(AgentToken & { secret: string }) | null>(null)
  const [revoking, setRevoking] = useState<string | null>(null)

  async function handleRevoke(tok: string) {
    setRevoking(tok)
    try {
      await revokeToken(tok)
      onRevoke(tok)
    } catch {
      // silently ignore — parent still holds stale state
    } finally {
      setRevoking(null)
    }
  }

  function handleCreated(t: AgentToken & { secret: string }) {
    setShowForm(false)
    setNewToken(t)
    onRevoke('__refresh__')
  }

  return (
    <div className="dash-card" data-testid="dashboard-tokens-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Agent tokens</h2>
        {!showForm && !newToken && (
          <button
            className="btn-action btn-action-primary"
            onClick={() => setShowForm(true)}
            data-testid="dashboard-create-token-button"
          >
            + Create token
          </button>
        )}
      </div>

      {newToken && (
        <NewTokenReveal
          token={newToken}
          onDone={() => setNewToken(null)}
        />
      )}

      {showForm && (
        <CreateTokenForm
          onCreated={handleCreated}
          onCancel={() => setShowForm(false)}
        />
      )}

      {tokens.length === 0 && !showForm && !newToken ? (
        <div className="dash-empty">No tokens yet. Create one to connect weftd agents.</div>
      ) : (
        <div className="dash-table-wrap">
          <table className="dash-table" data-testid="dashboard-tokens-table">
            <thead>
              <tr>
                <th>Token</th>
                <th>Label</th>
                <th>App</th>
                <th>Status</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {tokens.map(t => (
                <tr key={t.token} data-testid={`token-row-${t.token}`}>
                  <td>
                    <button
                      className="token-copy-btn"
                      title="Click to copy"
                      onClick={() => navigator.clipboard.writeText(t.token)}
                      data-testid={`token-copy-${t.token}`}
                    >
                      {truncateToken(t.token)}
                    </button>
                  </td>
                  <td className="token-label">{t.label}</td>
                  <td className="token-app">{t.app || '—'}</td>
                  <td>
                    <span className={`token-status token-status-${t.status}`}
                      data-testid={`token-status-${t.token}`}>
                      {t.status}
                    </span>
                  </td>
                  <td className="token-date">{formatDate(t.created_at)}</td>
                  <td>
                    {t.status === 'active' && (
                      <button
                        className="btn-action btn-action-danger"
                        onClick={() => handleRevoke(t.token)}
                        disabled={revoking === t.token}
                        data-testid={`token-revoke-${t.token}`}
                      >
                        {revoking === t.token ? '...' : 'Revoke'}
                      </button>
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

// ── Queries Section ─────────────────────────────────────────────────────────

interface QueriesCardProps {
  queries: SavedQuery[]
  onDelete: (id: string) => void
}

function QueriesCard({ queries, onDelete }: QueriesCardProps) {
  const navigate = useNavigate()
  const [deleting, setDeleting] = useState<string | null>(null)

  async function handleDelete(id: string) {
    setDeleting(id)
    try {
      await deleteQuery(id)
      onDelete(id)
    } catch {
      // ignore
    } finally {
      setDeleting(null)
    }
  }

  function runInShell(query: string) {
    navigate(`/shell?q=${encodeURIComponent(query)}`)
  }

  return (
    <div className="dash-card" data-testid="dashboard-queries-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Saved queries</h2>
      </div>

      {queries.length === 0 ? (
        <div className="dash-empty">
          No saved queries. Run a pipeline in the{' '}
          <a href="/shell" data-testid="dashboard-shell-link">shell</a>{' '}
          and save it for quick access.
        </div>
      ) : (
        <div className="queries-list">
          {queries.map(q => (
            <div key={q.id} className="query-row" data-testid={`query-row-${q.id}`}>
              <div className="query-meta">
                <div className="query-name">{q.name}</div>
                <div className="query-date">{formatDate(q.created_at)}</div>
              </div>
              <div className="query-pipeline" data-testid={`query-pipeline-${q.id}`}>
                {q.query}
              </div>
              <div className="query-actions">
                <button
                  className="btn-action btn-action-primary"
                  onClick={() => runInShell(q.query)}
                  data-testid={`query-run-${q.id}`}
                >
                  Run in shell →
                </button>
                <button
                  className="btn-action btn-action-danger"
                  onClick={() => handleDelete(q.id)}
                  disabled={deleting === q.id}
                  data-testid={`query-delete-${q.id}`}
                >
                  {deleting === q.id ? '...' : 'Delete'}
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// ── Main Dashboard ──────────────────────────────────────────────────────────

export default function Dashboard() {
  const [usage, setUsage] = useState<Usage | null>(null)
  const [tokens, setTokens] = useState<AgentToken[]>([])
  const [queries, setQueries] = useState<SavedQuery[]>([])
  const [loadError, setLoadError] = useState('')

  useEffect(() => {
    Promise.all([getUsage(), getTokens(), getQueries()])
      .then(([u, t, q]) => {
        setUsage(u)
        setTokens(t)
        setQueries(q)
      })
      .catch(err => {
        setLoadError(err instanceof Error ? err.message : 'Failed to load dashboard.')
      })
  }, [])

  function handleTokenAction(tok: string) {
    if (tok === '__refresh__') {
      getTokens().then(setTokens).catch(() => {})
    } else {
      setTokens(prev => prev.map(t =>
        t.token === tok ? { ...t, status: 'revoked' as const } : t
      ))
    }
  }

  function handleQueryDelete(id: string) {
    setQueries(prev => prev.filter(q => q.id !== id))
  }

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="dashboard-main">
        <div className="dash-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Dashboard</h1>
          </div>

          {loadError && (
            <div className="dash-load-error" data-testid="dashboard-load-error">
              {loadError}
            </div>
          )}

          {usage && <UsageCard usage={usage} />}

          <TokensCard tokens={tokens} onRevoke={handleTokenAction} />

          <QueriesCard queries={queries} onDelete={handleQueryDelete} />
        </div>
      </main>
    </div>
  )
}
