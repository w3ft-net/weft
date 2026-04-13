import { useState, useEffect } from 'react'
import SidebarNav from '../components/SidebarNav'
import {
  getNamespaces, createNamespace, getTokens, createToken, revokeToken,
  type NamespaceInfo, type AgentToken,
} from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import './Dashboard.css'

function truncateToken(tok: string): string {
  if (tok.length <= 16) return tok
  return `${tok.slice(0, 8)}...${tok.slice(-4)}`
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
  })
}

export default function Namespaces() {
  const { user } = useAuth()
  const [namespaces, setNamespaces] = useState<NamespaceInfo[]>([])
  const [tokens, setTokens] = useState<AgentToken[]>([])
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const [showTokenForm, setShowTokenForm] = useState<string | null>(null)
  const [tokenLabel, setTokenLabel] = useState('')
  const [tokenApp, setTokenApp] = useState('')
  const [newTokenSecret, setNewTokenSecret] = useState('')
  const [copied, setCopied] = useState(false)

  function loadData() {
    getNamespaces().then(setNamespaces).catch(() => {})
    getTokens().then(setTokens).catch(() => {})
  }

  useEffect(() => { loadData() }, [])

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    if (!newName.trim()) return
    setCreating(true)
    setError('')
    try {
      await createNamespace(newName.trim())
      setNewName('')
      setShowCreate(false)
      loadData()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create')
    } finally {
      setCreating(false)
    }
  }

  async function handleCreateToken(nsName: string) {
    if (!tokenLabel.trim()) return
    try {
      const resp = await createToken(tokenLabel.trim(), tokenApp.trim() || nsName)
      setNewTokenSecret(resp.token)
      setTokenLabel('')
      setTokenApp('')
      setShowTokenForm(null)
      loadData()
    } catch {
      // ignore
    }
  }

  async function handleRevoke(token: string) {
    await revokeToken(token).catch(() => {})
    loadData()
  }

  // Group tokens by namespace (using the app field prefix).
  function tokensForNamespace(nsName: string): AgentToken[] {
    return tokens.filter(t =>
      t.app === nsName || t.app.startsWith(nsName + '/')
    )
  }

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="namespaces-main">
        <div className="dash-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Namespaces</h1>
            {user && <div className="dash-empty" style={{ marginTop: 4 }}>{user.email}</div>}
          </div>

          {/* New token reveal */}
          {newTokenSecret && (
            <div className="token-reveal" data-testid="namespace-token-reveal">
              <div className="token-reveal-label">Token created — copy now, it won't be shown again.</div>
              <div className="token-reveal-value" data-testid="namespace-token-secret">{newTokenSecret}</div>
              <div className="form-actions">
                <button className="btn-action btn-action-primary" data-testid="namespace-token-copy-button"
                  onClick={() => { navigator.clipboard.writeText(newTokenSecret); setCopied(true); setTimeout(() => setCopied(false), 2000) }}>
                  {copied ? 'Copied!' : 'Copy to clipboard'}
                </button>
                <button className="btn-action" onClick={() => setNewTokenSecret('')}
                  data-testid="namespace-token-reveal-done-button">Done</button>
              </div>
            </div>
          )}

          <div className="dash-card" data-testid="namespaces-card">
            <div className="dash-card-header">
              <h2 className="dash-card-title">Namespaces</h2>
              {!showCreate && (
                <button className="btn-action btn-action-primary" onClick={() => setShowCreate(true)}
                  data-testid="namespace-create-button">+ Create</button>
              )}
            </div>

            {showCreate && (
              <form className="create-token-form" onSubmit={handleCreate} data-testid="namespace-create-form">
                <div className="form-row">
                  <div className="form-field">
                    <label className="form-label">Namespace name</label>
                    <input className="form-input" placeholder="e.g., weft, grantcentral"
                      value={newName} onChange={e => setNewName(e.target.value)}
                      data-testid="namespace-name-input" required autoFocus />
                  </div>
                </div>
                {error && <div className="form-error">{error}</div>}
                <div className="form-actions">
                  <button type="submit" className="btn-action btn-action-primary"
                    disabled={creating} data-testid="namespace-create-submit-button">
                    {creating ? 'Creating...' : 'Create'}
                  </button>
                  <button type="button" className="btn-action" onClick={() => { setShowCreate(false); setError('') }}
                    data-testid="namespace-create-cancel-button">Cancel</button>
                </div>
              </form>
            )}

            {namespaces.length === 0 && !showCreate ? (
              <div className="dash-empty">No namespaces yet. Create one to start ingesting logs.</div>
            ) : (
              <div className="func-list">
                {namespaces.map(ns => {
                  const nsTokens = tokensForNamespace(ns.name)
                  const isExpanded = expanded === ns.name

                  return (
                    <div key={ns.name} className="func-row" data-testid={`namespace-row-${ns.name}`}>
                      <button className="func-name" onClick={() => setExpanded(isExpanded ? null : ns.name)}
                        data-testid={`namespace-toggle-${ns.name}`}>
                        <span className="func-name-text">{ns.name}</span>
                        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                          <span className="watch-count-badge">{nsTokens.length} token{nsTokens.length !== 1 ? 's' : ''}</span>
                          <span className="func-chevron">{isExpanded ? '▾' : '▸'}</span>
                        </div>
                      </button>

                      {isExpanded && (
                        <div className="func-detail">
                          {/* Tokens for this namespace */}
                          {nsTokens.length > 0 && (
                            <div className="dash-table-wrap" style={{ marginBottom: 12 }}>
                              <table className="dash-table" data-testid={`namespace-tokens-${ns.name}`}>
                                <thead>
                                  <tr><th>Token</th><th>Label</th><th>App</th><th>Status</th><th>Created</th><th></th></tr>
                                </thead>
                                <tbody>
                                  {nsTokens.map(t => (
                                    <tr key={t.token} data-testid={`namespace-token-row-${t.token}`}>
                                      <td>
                                        <button className="token-copy-btn" title="Click to copy"
                                          onClick={() => navigator.clipboard.writeText(t.token)}
                                          data-testid={`namespace-token-copy-${t.token}`}>
                                          {truncateToken(t.token)}
                                        </button>
                                      </td>
                                      <td className="token-label">{t.label}</td>
                                      <td className="token-app">{t.app || '—'}</td>
                                      <td><span className={`token-status token-status-${t.status}`}>{t.status}</span></td>
                                      <td className="token-date">{formatDate(t.created_at)}</td>
                                      <td>
                                        {t.status === 'active' && (
                                          <button className="btn-action btn-action-danger"
                                            onClick={() => handleRevoke(t.token)}
                                            data-testid={`namespace-token-revoke-${t.token}`}>Revoke</button>
                                        )}
                                      </td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                            </div>
                          )}

                          {/* Create token form */}
                          {showTokenForm === ns.name ? (
                            <div className="create-token-form" style={{ marginTop: 8 }}>
                              <div className="form-row">
                                <div className="form-field">
                                  <label className="form-label">Label</label>
                                  <input className="form-input" placeholder="prod-web-01" value={tokenLabel}
                                    onChange={e => setTokenLabel(e.target.value)}
                                    data-testid={`namespace-token-label-${ns.name}`} />
                                </div>
                                <div className="form-field">
                                  <label className="form-label">Sub-app (optional)</label>
                                  <input className="form-input" placeholder={`${ns.name}/coordinator`} value={tokenApp}
                                    onChange={e => setTokenApp(e.target.value)}
                                    data-testid={`namespace-token-app-${ns.name}`} />
                                </div>
                              </div>
                              <div className="form-actions">
                                <button className="btn-action btn-action-primary"
                                  onClick={() => handleCreateToken(ns.name)}
                                  data-testid={`namespace-token-create-${ns.name}`}>Create token</button>
                                <button className="btn-action" onClick={() => setShowTokenForm(null)}>Cancel</button>
                              </div>
                            </div>
                          ) : (
                            <button className="btn-action btn-action-primary"
                              onClick={() => setShowTokenForm(ns.name)}
                              data-testid={`namespace-add-token-${ns.name}`}>+ Add token</button>
                          )}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </div>
        </div>
      </main>
    </div>
  )
}
