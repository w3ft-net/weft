import { useState, useEffect } from 'react'
import { useAuth } from '../lib/AuthContext'
import { Navigate } from 'react-router-dom'
import SidebarNav from '../components/SidebarNav'
import {
  adminListCustomers, adminGetCustomer, adminSetComp, adminSetTier,
  adminCreateCustomer,
  adminListBuckets, adminUpdateBucket, adminListShards,
  adminArchiveShard, adminDeleteShard,
  type AdminCustomer, type AdminCustomerDetail, type AdminCreatedCustomer,
  type AdminBucket, type AdminShard,
} from '../lib/api'
import './Dashboard.css'
import './Admin.css'

type Tab = 'customers' | 'buckets'

// ── Customers ─────────────────────────────────────────────────────────────

function CustomerDetail({ name, onClose }: { name: string; onClose: () => void }) {
  const [detail, setDetail] = useState<AdminCustomerDetail | null>(null)
  const [error, setError] = useState('')
  const [compTier, setCompTier] = useState('bolt')
  const [compReason, setCompReason] = useState('')
  const [tierSelect, setTierSelect] = useState('')
  const [saving, setSaving] = useState(false)

  function load() {
    adminGetCustomer(name).then(d => {
      setDetail(d)
      setTierSelect(d.subscription?.tier || '')
    }).catch(e => setError(e.message))
  }
  useEffect(() => { load() }, [name])

  if (error) return <div className="dash-load-error">{error}</div>
  if (!detail) return <div className="dash-empty">loading...</div>

  const sub = detail.subscription
  const isComp = sub?.is_comp ?? false

  async function handleToggleComp() {
    setSaving(true)
    try {
      if (isComp) {
        await adminSetComp(name, false)
      } else {
        await adminSetComp(name, true, compTier, compReason || undefined)
      }
      load()
    } catch (e: any) { setError(e.message) }
    finally { setSaving(false) }
  }

  async function handleSetTier() {
    if (!tierSelect) return
    setSaving(true)
    try {
      await adminSetTier(name, tierSelect)
      load()
    } catch (e: any) { setError(e.message) }
    finally { setSaving(false) }
  }

  return (
    <div className="dash-card" data-testid="admin-customer-detail">
      <div className="dash-card-header">
        <h2 className="dash-card-title">{detail.display_name || detail.name}</h2>
        <button className="btn-action" onClick={onClose} data-testid="admin-customer-close-button">Close</button>
      </div>

      <div className="usage-stats">
        <div className="usage-stat">
          <div className="usage-stat-label">Name</div>
          <div className="usage-stat-value" style={{ fontSize: '1rem' }}>{detail.name}</div>
        </div>
        <div className="usage-stat">
          <div className="usage-stat-label">Tier</div>
          <div className="usage-stat-value" style={{ fontSize: '1rem' }}>
            {sub ? sub.tier : 'none'}
            {isComp && <span className="admin-comp-badge">comp</span>}
          </div>
        </div>
        <div className="usage-stat">
          <div className="usage-stat-label">Status</div>
          <div className="usage-stat-value" style={{ fontSize: '1rem' }}>{sub?.status || 'inactive'}</div>
        </div>
        <div className="usage-stat">
          <div className="usage-stat-label">Created</div>
          <div className="usage-stat-value" style={{ fontSize: '1rem' }}>
            {new Date(detail.created_at).toLocaleDateString()}
          </div>
        </div>
      </div>

      {/* Namespaces */}
      {detail.namespaces.length > 0 && (
        <div style={{ marginBottom: 16 }}>
          <div className="form-label" style={{ marginBottom: 8 }}>Namespaces</div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            {detail.namespaces.map(ns => (
              <span key={ns.id} className="dash-tier-badge" style={{ fontSize: '0.7rem' }}>{ns.name}</span>
            ))}
          </div>
        </div>
      )}

      {/* Team */}
      {detail.team.length > 0 && (
        <div style={{ marginBottom: 20 }}>
          <div className="form-label" style={{ marginBottom: 8 }}>Team</div>
          <div className="dash-table-wrap">
            <table className="dash-table" data-testid="admin-customer-team-table">
              <thead><tr><th>Email</th><th>Role</th><th>Status</th></tr></thead>
              <tbody>
                {detail.team.map(m => (
                  <tr key={m.email}>
                    <td className="token-label">{m.email}</td>
                    <td><span className={`token-status ${m.role === 'admin' ? 'token-status-active' : ''}`}>{m.role}</span></td>
                    <td><span className={`token-status ${m.status === 'active' ? 'token-status-active' : 'token-status-revoked'}`}>{m.status}</span></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Comp controls */}
      <div className="create-token-form" data-testid="admin-customer-comp-form">
        <div className="form-label" style={{ marginBottom: 12 }}>Comp subscription</div>
        {!isComp && (
          <div className="form-row">
            <div className="form-field">
              <label className="form-label">Tier</label>
              <select className="form-input" value={compTier} onChange={e => setCompTier(e.target.value)}
                data-testid="admin-comp-tier-select">
                <option value="swatch">swatch</option>
                <option value="yard">yard</option>
                <option value="bolt">bolt</option>
              </select>
            </div>
            <div className="form-field">
              <label className="form-label">Reason</label>
              <input className="form-input" placeholder="e.g. beta-tester" value={compReason}
                onChange={e => setCompReason(e.target.value)} data-testid="admin-comp-reason-input" />
            </div>
          </div>
        )}
        {isComp && sub?.comp_reason && (
          <div className="dash-empty" style={{ padding: '0 0 12px' }}>Reason: {sub.comp_reason}</div>
        )}
        <button
          className={`btn-action ${isComp ? 'btn-action-danger' : 'btn-action-primary'}`}
          onClick={handleToggleComp}
          disabled={saving}
          data-testid="admin-comp-toggle-button"
        >
          {saving ? '...' : isComp ? 'Revoke comp' : 'Grant comp'}
        </button>
      </div>

      {/* Tier override */}
      <div className="create-token-form" style={{ marginTop: 12 }} data-testid="admin-customer-tier-form">
        <div className="form-label" style={{ marginBottom: 12 }}>Set tier</div>
        <div className="form-row">
          <div className="form-field">
            <select className="form-input" value={tierSelect} onChange={e => setTierSelect(e.target.value)}
              data-testid="admin-tier-select">
              <option value="">—</option>
              <option value="swatch">swatch</option>
              <option value="yard">yard</option>
              <option value="bolt">bolt</option>
            </select>
          </div>
          <div className="form-field" style={{ flex: 0 }}>
            <button className="btn-action btn-action-primary" onClick={handleSetTier}
              disabled={saving || !tierSelect} data-testid="admin-tier-set-button">
              {saving ? '...' : 'Set'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}

// NewCustomerForm provisions a comp account (org + verified owner + comp
// subscription) — the operator replacement for public signup. Collapsed
// by default; on success it surfaces the owner's login link (also emailed
// by the server) and refreshes the customer list.
function NewCustomerForm({ onCreated }: { onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [email, setEmail] = useState('')
  const [orgName, setOrgName] = useState('')
  const [tier, setTier] = useState('bolt')
  const [reason, setReason] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [created, setCreated] = useState<AdminCreatedCustomer | null>(null)

  function reset() {
    setEmail(''); setOrgName(''); setTier('bolt'); setReason(''); setError('')
  }

  async function handleCreate() {
    if (!email.trim() || !orgName.trim()) {
      setError('Email and org name are required.')
      return
    }
    setSaving(true)
    setError('')
    try {
      const res = await adminCreateCustomer(email.trim(), orgName.trim(), tier, reason.trim() || undefined)
      setCreated(res)
      reset()
      onCreated()
    } catch (e: any) { setError(e.message) }
    finally { setSaving(false) }
  }

  if (!open) {
    return (
      <div className="dash-card" data-testid="admin-new-customer-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">New comp account</h2>
          <button className="btn-action btn-action-primary" onClick={() => { setOpen(true); setCreated(null) }}
            data-testid="admin-new-customer-open-button">New account</button>
        </div>
        {created && (
          <div className="dash-empty" style={{ padding: '0 0 4px' }} data-testid="admin-new-customer-result">
            Created <strong>{created.name}</strong> ({created.comp_reason} / {created.tier}) for {created.owner_email}.
            {created.login_url && (
              <div style={{ marginTop: 6 }}>
                Login link: <a href={created.login_url} data-testid="admin-new-customer-login-link">{created.login_url}</a>
              </div>
            )}
          </div>
        )}
      </div>
    )
  }

  return (
    <div className="dash-card" data-testid="admin-new-customer-card">
      <div className="dash-card-header">
        <h2 className="dash-card-title">New comp account</h2>
        <button className="btn-action" onClick={() => { setOpen(false); reset() }}
          data-testid="admin-new-customer-cancel-button">Cancel</button>
      </div>
      <div className="create-token-form" data-testid="admin-new-customer-form">
        <div className="form-row">
          <div className="form-field">
            <label className="form-label">Owner email</label>
            <input className="form-input" type="email" placeholder="owner@acme.com" value={email}
              onChange={e => setEmail(e.target.value)} data-testid="admin-new-customer-email-input" />
          </div>
          <div className="form-field">
            <label className="form-label">Org name</label>
            <input className="form-input" placeholder="acme" value={orgName}
              onChange={e => setOrgName(e.target.value)} data-testid="admin-new-customer-org-input" />
          </div>
        </div>
        <div className="form-row">
          <div className="form-field">
            <label className="form-label">Tier</label>
            <select className="form-input" value={tier} onChange={e => setTier(e.target.value)}
              data-testid="admin-new-customer-tier-select">
              <option value="swatch">swatch</option>
              <option value="yard">yard</option>
              <option value="bolt">bolt</option>
            </select>
          </div>
          <div className="form-field">
            <label className="form-label">Reason</label>
            <input className="form-input" placeholder="e.g. design-partner" value={reason}
              onChange={e => setReason(e.target.value)} data-testid="admin-new-customer-reason-input" />
          </div>
        </div>
        <div className="form-actions">
          <button className="btn-action btn-action-primary" onClick={handleCreate} disabled={saving}
            data-testid="admin-new-customer-create-button">
            {saving ? 'Creating...' : 'Create comp account'}
          </button>
        </div>
        {error && <div className="form-error" style={{ marginTop: 8 }}>{error}</div>}
      </div>
    </div>
  )
}

function CustomersTab() {
  const [customers, setCustomers] = useState<AdminCustomer[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [error, setError] = useState('')

  function load() {
    adminListCustomers().then(setCustomers).catch(e => setError(e.message))
  }
  useEffect(() => { load() }, [])

  if (error) return <div className="dash-load-error">{error}</div>

  return (
    <>
      <NewCustomerForm onCreated={load} />
      <div className="dash-card" data-testid="admin-customers-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Customers</h2>
          <span className="watch-count-badge">{customers.length}</span>
        </div>
        {customers.length === 0 ? (
          <div className="dash-empty">No customers yet.</div>
        ) : (
          <div className="dash-table-wrap">
            <table className="dash-table" data-testid="admin-customers-table">
              <thead>
                <tr><th>Name</th><th>Display</th><th>Tier</th><th>Status</th><th>Created</th></tr>
              </thead>
              <tbody>
                {customers.map(c => (
                  <tr
                    key={c.id}
                    onClick={() => setSelected(selected === c.name ? null : c.name)}
                    style={{ cursor: 'pointer' }}
                    className={selected === c.name ? 'admin-row-selected' : ''}
                    data-testid={`admin-customer-row-${c.name}`}
                  >
                    <td className="token-label">{c.name}</td>
                    <td>{c.display_name}</td>
                    <td>
                      <span className="dash-tier-badge" style={{ fontSize: '0.68rem' }}>{c.tier}</span>
                      {c.is_comp && <span className="admin-comp-badge">comp</span>}
                    </td>
                    <td>
                      <span className={`token-status ${c.status === 'active' ? 'token-status-active' : 'token-status-revoked'}`}>
                        {c.status}
                      </span>
                    </td>
                    <td className="token-date">{new Date(c.created_at).toLocaleDateString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {selected && <CustomerDetail name={selected} onClose={() => setSelected(null)} />}
    </>
  )
}

// ── Buckets ───────────────────────────────────────────────────────────────

function ShardRow({ shard, onRefresh }: { shard: AdminShard; onRefresh: () => void }) {
  const [confirming, setConfirming] = useState<'archive' | 'delete' | 'force-delete' | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function handleArchive() {
    setBusy(true)
    setError('')
    try {
      await adminArchiveShard(shard.ID)
      setConfirming(null)
      onRefresh()
    } catch (e: any) { setError(e.message) }
    finally { setBusy(false) }
  }

  async function handleDelete(force: boolean) {
    setBusy(true)
    setError('')
    try {
      await adminDeleteShard(shard.ID, force)
      setConfirming(null)
      onRefresh()
    } catch (e: any) {
      if (e.message?.includes('retention') || e.message?.includes('requires_force')) {
        setConfirming('force-delete')
        setError('Shard is within retention window.')
      } else {
        setError(e.message)
      }
    }
    finally { setBusy(false) }
  }

  return (
    <>
      <tr data-testid={`admin-shard-row-${shard.ID}`}>
        <td className="token-label" style={{ maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis' }}
          title={shard.ID}>{shard.ID}</td>
        <td>
          <span className={`token-status ${shard.State === 'INGESTING' ? 'token-status-active' : shard.State === 'ARCHIVED' ? 'token-status-revoked' : ''}`}>
            {shard.State}
          </span>
        </td>
        <td className="token-date">{shard.WindowStart?.slice(0, 10)}</td>
        <td className="token-date">{shard.WindowEnd?.slice(0, 10)}</td>
        <td>{shard.RowCount.toLocaleString()}</td>
        <td>
          <div style={{ display: 'flex', gap: 6 }}>
            {shard.State === 'READ_ONLY' && !confirming && (
              <button className="btn-action" onClick={() => setConfirming('archive')}
                data-testid={`admin-shard-archive-${shard.ID}-button`}>Archive</button>
            )}
            {shard.State === 'ARCHIVED' && !confirming && (
              <button className="btn-action btn-action-danger" onClick={() => setConfirming('delete')}
                data-testid={`admin-shard-delete-${shard.ID}-button`}>Delete</button>
            )}
            {confirming === 'archive' && (
              <>
                <button className="btn-action btn-action-primary" onClick={handleArchive} disabled={busy}
                  data-testid={`admin-shard-archive-confirm-${shard.ID}-button`}>
                  {busy ? '...' : 'Confirm archive'}
                </button>
                <button className="btn-action" onClick={() => setConfirming(null)}>Cancel</button>
              </>
            )}
            {confirming === 'delete' && (
              <>
                <button className="btn-action btn-action-danger" onClick={() => handleDelete(false)} disabled={busy}
                  data-testid={`admin-shard-delete-confirm-${shard.ID}-button`}>
                  {busy ? '...' : 'Confirm delete'}
                </button>
                <button className="btn-action" onClick={() => setConfirming(null)}>Cancel</button>
              </>
            )}
            {confirming === 'force-delete' && (
              <>
                <button className="btn-action btn-action-danger" onClick={() => handleDelete(true)} disabled={busy}
                  data-testid={`admin-shard-force-delete-${shard.ID}-button`}>
                  {busy ? '...' : 'Force delete'}
                </button>
                <button className="btn-action" onClick={() => setConfirming(null)}>Cancel</button>
              </>
            )}
          </div>
        </td>
      </tr>
      {error && (
        <tr><td colSpan={6} className="form-error" style={{ padding: '4px 12px' }}>{error}</td></tr>
      )}
    </>
  )
}

function BucketDetail({ bucket, onClose }: { bucket: AdminBucket; onClose: () => void }) {
  const [shards, setShards] = useState<AdminShard[]>([])
  const [error, setError] = useState('')
  const [editing, setEditing] = useState(false)
  const [windowDur, setWindowDur] = useState(bucket.WindowDuration)
  const [targetCount, setTargetCount] = useState(String(bucket.TargetIngestingCount))
  const [tier, setTier] = useState(bucket.Tier)
  const [saving, setSaving] = useState(false)

  function loadShards() {
    adminListShards(bucket.ID).then(setShards).catch(e => setError(e.message))
  }
  useEffect(() => { loadShards() }, [bucket.ID])

  async function handleSave() {
    setSaving(true)
    try {
      await adminUpdateBucket(bucket.ID, {
        WindowDuration: windowDur,
        TargetIngestingCount: parseInt(targetCount, 10) || 1,
        Tier: tier,
      })
      setEditing(false)
      onClose()
    } catch (e: any) { setError(e.message) }
    finally { setSaving(false) }
  }

  // Empty Services is the catch-all bucket; show a sentinel badge so it
  // doesn't render as an empty row.
  const services = bucket.Services.length > 0 ? bucket.Services : ['(catch-all)']

  return (
    <div className="dash-card" data-testid="admin-bucket-detail">
      <div className="dash-card-header">
        <h2 className="dash-card-title">Bucket #{bucket.ID}</h2>
        <div style={{ display: 'flex', gap: 8 }}>
          {!editing && (
            <button className="btn-action btn-action-primary" onClick={() => setEditing(true)}
              data-testid="admin-bucket-edit-button">Edit</button>
          )}
          <button className="btn-action" onClick={onClose} data-testid="admin-bucket-close-button">Close</button>
        </div>
      </div>

      <div style={{ display: 'flex', gap: 8, marginBottom: 16, flexWrap: 'wrap' }}>
        {services.map(a => (
          <span key={a} className="dash-tier-badge" style={{ fontSize: '0.68rem' }}>{a}</span>
        ))}
      </div>

      {editing ? (
        <div className="create-token-form" data-testid="admin-bucket-edit-form">
          <div className="form-row">
            <div className="form-field">
              <label className="form-label">Window duration</label>
              <input className="form-input" value={windowDur} onChange={e => setWindowDur(e.target.value)}
                data-testid="admin-bucket-window-input" />
            </div>
            <div className="form-field">
              <label className="form-label">Target ingesting</label>
              <input className="form-input" type="number" min="1" value={targetCount}
                onChange={e => setTargetCount(e.target.value)}
                data-testid="admin-bucket-target-input" />
            </div>
            <div className="form-field">
              <label className="form-label">Tier</label>
              <select className="form-input" value={tier} onChange={e => setTier(e.target.value)}
                data-testid="admin-bucket-tier-select">
                <option value="small">small</option>
                <option value="medium">medium</option>
                <option value="large">large</option>
              </select>
            </div>
          </div>
          <div className="form-actions">
            <button className="btn-action btn-action-primary" onClick={handleSave} disabled={saving}
              data-testid="admin-bucket-save-button">{saving ? 'Saving...' : 'Save'}</button>
            <button className="btn-action" onClick={() => setEditing(false)}>Cancel</button>
          </div>
          {error && <div className="form-error" style={{ marginTop: 8 }}>{error}</div>}
        </div>
      ) : (
        <div className="usage-stats" style={{ marginBottom: 20 }}>
          <div className="usage-stat">
            <div className="usage-stat-label">Window</div>
            <div className="usage-stat-value" style={{ fontSize: '1rem' }}>{bucket.WindowDuration}</div>
          </div>
          <div className="usage-stat">
            <div className="usage-stat-label">Target ingesting</div>
            <div className="usage-stat-value" style={{ fontSize: '1rem' }}>{bucket.TargetIngestingCount}</div>
          </div>
          <div className="usage-stat">
            <div className="usage-stat-label">Tier</div>
            <div className="usage-stat-value" style={{ fontSize: '1rem' }}>{bucket.Tier}</div>
          </div>
        </div>
      )}

      {/* Shards drilldown */}
      <div style={{ marginTop: 12 }}>
        <div className="form-label" style={{ marginBottom: 8 }}>Shards ({shards.length})</div>
        {shards.length === 0 ? (
          <div className="dash-empty">No shards in this bucket.</div>
        ) : (
          <div className="dash-table-wrap">
            <table className="dash-table" data-testid="admin-shards-table">
              <thead>
                <tr><th>ID</th><th>State</th><th>Start</th><th>End</th><th>Rows</th><th></th></tr>
              </thead>
              <tbody>
                {shards.map(s => (
                  <ShardRow key={s.ID} shard={s} onRefresh={loadShards} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  )
}

function BucketsTab() {
  const [buckets, setBuckets] = useState<AdminBucket[]>([])
  const [selected, setSelected] = useState<AdminBucket | null>(null)
  const [error, setError] = useState('')

  function load() {
    adminListBuckets().then(setBuckets).catch(e => setError(e.message))
  }
  useEffect(() => { load() }, [])

  if (error) return <div className="dash-load-error">{error}</div>

  return (
    <>
      <div className="dash-card" data-testid="admin-buckets-card">
        <div className="dash-card-header">
          <h2 className="dash-card-title">Buckets</h2>
          <span className="watch-count-badge">{buckets.length}</span>
        </div>
        {buckets.length === 0 ? (
          <div className="dash-empty">No buckets.</div>
        ) : (
          <div className="dash-table-wrap">
            <table className="dash-table" data-testid="admin-buckets-table">
              <thead>
                <tr><th>ID</th><th>Services</th><th>Window</th><th>Target</th><th>Tier</th></tr>
              </thead>
              <tbody>
                {buckets.map(b => {
                  const services = b.Services.length > 0 ? b.Services.join(', ') : '(catch-all)'
                  return (
                    <tr
                      key={b.ID}
                      onClick={() => setSelected(selected?.ID === b.ID ? null : b)}
                      style={{ cursor: 'pointer' }}
                      className={selected?.ID === b.ID ? 'admin-row-selected' : ''}
                      data-testid={`admin-bucket-row-${b.ID}`}
                    >
                      <td className="token-label">{b.ID}</td>
                      <td style={{ maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis' }}>{services}</td>
                      <td>{b.WindowDuration}</td>
                      <td>{b.TargetIngestingCount}</td>
                      <td><span className="dash-tier-badge" style={{ fontSize: '0.68rem' }}>{b.Tier}</span></td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {selected && <BucketDetail bucket={selected} onClose={() => { setSelected(null); load() }} />}
    </>
  )
}

// ── Main ──────────────────────────────────────────────────────────────────

export default function Admin() {
  const { user, loading } = useAuth()
  const [tab, setTab] = useState<Tab>('customers')

  if (loading) return null
  if (!user?.is_superadmin) return <Navigate to="/dashboards" replace />

  return (
    <div className="app-layout">
      <SidebarNav />
      <main className="app-main" data-testid="admin-main">
        <div className="dash-content">
          <div className="dash-header">
            <h1 className="dash-page-title">Admin</h1>
          </div>

          <div className="admin-tabs" data-testid="admin-tabs">
            <button
              className={`admin-tab${tab === 'customers' ? ' admin-tab-active' : ''}`}
              onClick={() => setTab('customers')}
              data-testid="admin-customers-tab"
            >
              Customers
            </button>
            <button
              className={`admin-tab${tab === 'buckets' ? ' admin-tab-active' : ''}`}
              onClick={() => setTab('buckets')}
              data-testid="admin-buckets-tab"
            >
              Buckets
            </button>
          </div>

          {tab === 'customers' && <CustomersTab />}
          {tab === 'buckets' && <BucketsTab />}
        </div>
      </main>
    </div>
  )
}
