import { useState, useEffect } from 'react'
import SidebarNav from '../components/SidebarNav'
import Pane from '../components/Pane'
import PendingSourceConfigure from '../components/PendingSourceConfigure'
import AddKinesisSource from '../components/AddKinesisSource'
import ConfiguredSourcesList from '../components/ConfiguredSourcesList'
import {
  getNamespaces, createNamespace, getTokens, createToken, revokeToken,
  getPendingSources, onPendingSourcesChanged, listSources,
  type NamespaceInfo, type AgentToken, type PendingSource, type SourceInfo,
} from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import './Dashboard.css'
import './Namespaces.css'

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
  })
}

export default function Namespaces() {
  const { user } = useAuth()
  const [namespaces, setNamespaces] = useState<NamespaceInfo[]>([])
  const [tokens, setTokens] = useState<AgentToken[]>([])
  const [pendingSources, setPendingSources] = useState<PendingSource[]>([])
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const [showTokenForm, setShowTokenForm] = useState<string | null>(null)
  const [tokenLabel, setTokenLabel] = useState('')
  const [tokenEnv, setTokenEnv] = useState('')
  const [newTokenSecret, setNewTokenSecret] = useState('')
  const [copied, setCopied] = useState(false)
  const [sources, setSources] = useState<SourceInfo[]>([])
  const [showAddKinesis, setShowAddKinesis] = useState<string | null>(null)

  function loadData() {
    getNamespaces().then(setNamespaces).catch(() => {})
    getTokens().then(setTokens).catch(() => {})
    getPendingSources().then(setPendingSources).catch(() => {})
    listSources().then(setSources).catch(() => {})
  }

  function sourcesForNamespace(nsName: string): SourceInfo[] {
    return sources.filter(s => s.namespace === nsName)
  }

  useEffect(() => {
    loadData()
    return onPendingSourcesChanged(() => {
      getPendingSources().then(setPendingSources).catch(() => {})
    })
  }, [])

  // Auto-expand any namespace that has pending sources so the user sees
  // the prompt on first load without having to click around.
  useEffect(() => {
    if (expanded !== null) return
    const nsWithPending = pendingSources[0]?.namespace
    if (nsWithPending) setExpanded(nsWithPending)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pendingSources.length])

  function pendingForNamespace(nsName: string): PendingSource[] {
    return pendingSources.filter(p => p.namespace === nsName)
  }

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
      const resp = await createToken(tokenLabel.trim(), nsName, tokenEnv.trim())
      setNewTokenSecret(resp.token)
      setTokenLabel('')
      setTokenEnv('')
      setShowTokenForm(null)
      loadData()
    } catch {
      // ignore
    }
  }

  async function handleRevoke(id: string) {
    await revokeToken(id).catch(() => {})
    loadData()
  }

  function tokensForNamespace(nsName: string): AgentToken[] {
    return tokens.filter(t => t.namespace === nsName)
  }

  const pendingTotal = pendingSources.length

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="namespaces-main">
        <div className="ns-content">
          <header className="ns-header">
            <h1 className="ns-title"><span className="tui-dim">~/</span>namespaces</h1>
            {user && <span className="ns-who">{user.email}</span>}
          </header>

          {/* New token reveal */}
          {newTokenSecret && (
            <Pane title="token created" className="ns-reveal" data-testid="namespace-token-reveal">
              <p className="ns-reveal-note">copy it now — it won't be shown again.</p>
              <div className="ns-reveal-kv">
                <span className="ns-key">token:</span>
                <code className="ns-reveal-value" data-testid="namespace-token-secret">{newTokenSecret}</code>
              </div>
              <div className="ns-actions">
                <button className="tui-btn tui-btn-primary" data-testid="namespace-token-copy-button"
                  onClick={() => { navigator.clipboard.writeText(newTokenSecret); setCopied(true); setTimeout(() => setCopied(false), 2000) }}>
                  {copied ? 'copied' : 'copy to clipboard'}
                </button>
                <button className="tui-btn" onClick={() => setNewTokenSecret('')}
                  data-testid="namespace-token-reveal-done-button">done</button>
              </div>
            </Pane>
          )}

          <Pane
            title="namespaces"
            meta={namespaces.length > 0 ? `${namespaces.length}` : undefined}
            actions={!showCreate && (
              <button className="tui-btn tui-btn-primary" onClick={() => setShowCreate(true)}
                data-testid="namespace-create-button">new</button>
            )}
            data-testid="namespaces-card"
          >
            {showCreate && (
              <form className="ns-form" onSubmit={handleCreate} data-testid="namespace-create-form">
                <label className="ns-field">
                  <span className="ns-key">name:</span>
                  <input placeholder="e.g. weft, grantcentral"
                    value={newName} onChange={e => setNewName(e.target.value)}
                    data-testid="namespace-name-input" required autoFocus />
                </label>
                {error && <div className="ns-error">error: {error}</div>}
                <div className="ns-actions">
                  <button type="submit" className="tui-btn tui-btn-primary"
                    disabled={creating} data-testid="namespace-create-submit-button">
                    {creating ? 'creating…' : 'create'}
                  </button>
                  <button type="button" className="tui-btn" onClick={() => { setShowCreate(false); setError('') }}
                    data-testid="namespace-create-cancel-button">cancel</button>
                </div>
              </form>
            )}

            {namespaces.length === 0 && !showCreate ? (
              <div className="ns-empty tui-cursor">no namespaces yet. create one to start ingesting logs.</div>
            ) : (
              <div className="ns-list">
                {namespaces.map(ns => {
                  const nsTokens = tokensForNamespace(ns.name)
                  const nsPending = pendingForNamespace(ns.name)
                  const nsSources = sourcesForNamespace(ns.name)
                  const isExpanded = expanded === ns.name

                  return (
                    <div key={ns.name} className={`ns-row${isExpanded ? ' ns-row-open' : ''}`} data-testid={`namespace-row-${ns.name}`}>
                      <button className="ns-toggle" onClick={() => setExpanded(isExpanded ? null : ns.name)}
                        aria-expanded={isExpanded}
                        data-testid={`namespace-toggle-${ns.name}`}>
                        <span className="ns-chevron">{isExpanded ? '▾' : '▸'}</span>
                        <span className="ns-name">{ns.name}/</span>
                        <span className="ns-count">
                          {nsPending.length > 0 && (
                            <span className="ns-pending" data-testid={`namespace-pending-badge-${ns.name}`}>
                              +{nsPending.length} pending
                            </span>
                          )}
                        </span>
                        <span className="ns-count">{nsSources.length} source{nsSources.length !== 1 ? 's' : ''}</span>
                        <span className="ns-count">{nsTokens.length} token{nsTokens.length !== 1 ? 's' : ''}</span>
                      </button>

                      {isExpanded && (
                        <div className="ns-detail">
                          {/* Pending sources — shown first so they're the obvious thing to act on */}
                          {nsPending.length > 0 && (
                            <Pane
                              title="pending sources"
                              meta={`${nsPending.length}`}
                              className="ns-sub ns-sub-pending"
                              data-testid={`namespace-pending-${ns.name}`}
                            >
                              {nsPending.map(src => (
                                <PendingSourceConfigure
                                  key={`${src.namespace}/${src.service}`}
                                  source={src}
                                  onConfigured={loadData}
                                />
                              ))}
                            </Pane>
                          )}

                          {/* Configured sources (CloudWatch push + weftd) */}
                          <Pane
                            title="sources"
                            className="ns-sub"
                            actions={showAddKinesis !== ns.name && (
                              <button
                                className="tui-btn"
                                onClick={() => setShowAddKinesis(ns.name)}
                                data-testid={`namespace-add-kinesis-${ns.name}-button`}
                              >
                                add kinesis source
                              </button>
                            )}
                          >
                            {nsSources.length === 0 && showAddKinesis !== ns.name && (
                              <div className="ns-empty">no sources configured.</div>
                            )}
                            <ConfiguredSourcesList
                              namespace={ns.name}
                              sources={nsSources}
                              onChanged={loadData}
                            />
                            {showAddKinesis === ns.name && (
                              <AddKinesisSource
                                namespace={ns.name}
                                onAdded={() => { setShowAddKinesis(null); loadData() }}
                                onCancel={() => setShowAddKinesis(null)}
                              />
                            )}
                          </Pane>

                          {/* Tokens for this namespace.
                              The raw token is no longer retrievable after
                              creation (hashed at rest). Show the public id + label
                              so humans can identify tokens; revoke by id. */}
                          <Pane
                            title="tokens"
                            className="ns-sub"
                            actions={showTokenForm !== ns.name && (
                              <button className="tui-btn tui-btn-primary"
                                onClick={() => setShowTokenForm(ns.name)}
                                data-testid={`namespace-add-token-${ns.name}`}>new token</button>
                            )}
                          >
                            {nsTokens.length === 0 && showTokenForm !== ns.name && (
                              <div className="ns-empty">no tokens. create one to connect weftd or a log stream.</div>
                            )}
                            {nsTokens.length > 0 && (
                              <div className="ns-table-wrap">
                                <table className="tui-table" data-testid={`namespace-tokens-${ns.name}`}>
                                  <thead>
                                    <tr><th>id</th><th>label</th><th>env</th><th>status</th><th>created</th><th></th></tr>
                                  </thead>
                                  <tbody>
                                    {nsTokens.map(t => (
                                      <tr key={t.id} className={t.status === 'revoked' ? 'ns-token-revoked' : undefined} data-testid={`namespace-token-row-${t.id}`}>
                                        <td className="ns-token-prefix">{t.id}</td>
                                        <td className="ns-token-label">{t.label}</td>
                                        <td>{t.env || <span className="tui-dim">-</span>}</td>
                                        <td>
                                          <span className={`tui-tag ${t.status === 'active' ? 'tui-tag-ok' : ''}`}>{t.status}</span>
                                        </td>
                                        <td className="tui-dim">{formatDate(t.created_at)}</td>
                                        <td className="ns-token-action">
                                          {t.status === 'active' && (
                                            <button className="tui-btn tui-btn-danger"
                                              onClick={() => handleRevoke(t.id)}
                                              data-testid={`namespace-token-revoke-${t.id}`}>revoke</button>
                                          )}
                                        </td>
                                      </tr>
                                    ))}
                                  </tbody>
                                </table>
                              </div>
                            )}

                            {/* Create token form */}
                            {showTokenForm === ns.name && (
                              <div className="ns-form ns-form-token">
                                <label className="ns-field">
                                  <span className="ns-key">label:</span>
                                  <input placeholder="prod-web-01" value={tokenLabel}
                                    onChange={e => setTokenLabel(e.target.value)}
                                    data-testid={`namespace-token-label-${ns.name}`} />
                                </label>
                                <label className="ns-field">
                                  <span className="ns-key">env:</span>
                                  <input placeholder="prod (optional)" value={tokenEnv}
                                    onChange={e => setTokenEnv(e.target.value)}
                                    data-testid={`namespace-token-env-${ns.name}`} />
                                </label>
                                <div className="ns-actions">
                                  <button className="tui-btn tui-btn-primary"
                                    onClick={() => handleCreateToken(ns.name)}
                                    data-testid={`namespace-token-create-${ns.name}`}>create token</button>
                                  <button className="tui-btn" onClick={() => { setShowTokenForm(null); setTokenEnv('') }}
                                    data-testid={`namespace-token-cancel-${ns.name}-button`}>cancel</button>
                                </div>
                              </div>
                            )}
                          </Pane>
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </Pane>

          {pendingTotal > 0 && (
            <p className="ns-footnote">
              <span className="tui-hi">+{pendingTotal}</span> source{pendingTotal === 1 ? '' : 's'} waiting for a schema — expand the namespace to confirm.
            </p>
          )}
        </div>
      </main>
    </div>
  )
}
