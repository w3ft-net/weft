import { useState } from 'react'
import { useAuth } from '../lib/AuthContext'
import { apiFetch } from '../lib/api'
import './Dashboard.css'

export default function Device() {
  const { user } = useAuth()
  const [code, setCode] = useState('')
  const [status, setStatus] = useState<'ready' | 'approving' | 'done' | 'error'>('ready')
  const [error, setError] = useState('')

  async function handleApprove() {
    setStatus('approving')
    try {
      await apiFetch('/api/auth/device/approve', {
        method: 'POST',
        body: JSON.stringify({ user_code: code.trim() }),
      })
      setStatus('done')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to approve')
      setStatus('error')
    }
  }

  return (
    <div className="app-layout">
      <main className="app-main" data-testid="device-main">
        <div className="dash-content" style={{ maxWidth: 480, paddingTop: 80 }}>
          <div className="dash-card">
            <div className="dash-card-header">
              <h2 className="dash-card-title">Authorize CLI</h2>
            </div>

            {status === 'ready' && (
              <>
                <p style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem', color: 'var(--text-secondary)', marginBottom: 16 }}>
                  The weft CLI is requesting access to your account
                  {user ? ` (${user.email})` : ''}.
                </p>
                <p style={{ fontFamily: 'var(--font-mono)', fontSize: '0.85rem', color: 'var(--text-dim)', marginBottom: 20 }}>
                  Enter the code shown in your terminal. Only authorize a code you can see printed
                  by a `weft login` you ran yourself — never one from a link someone sent you.
                </p>
                <input
                  type="text"
                  value={code}
                  onChange={(e) => setCode(e.target.value.toUpperCase())}
                  placeholder="XXXX-XX"
                  autoFocus
                  style={{
                    fontFamily: 'var(--font-mono)', fontSize: '2rem', fontWeight: 700,
                    color: 'var(--accent)', textAlign: 'center', padding: '16px 0', marginBottom: 20,
                    letterSpacing: '0.1em', width: '100%', background: 'transparent',
                    border: '1px solid var(--border)', borderRadius: 6,
                  }}
                  data-testid="device-user-code-input"
                />
                <button
                  className="btn-action btn-action-primary"
                  style={{ width: '100%', padding: '12px' }}
                  onClick={handleApprove}
                  disabled={!code.trim()}
                  data-testid="device-approve-button"
                >
                  Authorize
                </button>
              </>
            )}

            {status === 'approving' && (
              <div className="dash-empty">Authorizing...</div>
            )}

            {status === 'done' && (
              <div style={{
                fontFamily: 'var(--font-mono)', fontSize: '0.85rem',
                color: 'var(--green)', padding: '20px 0',
              }}>
                CLI authorized. You can close this tab — the CLI will pick it up automatically.
              </div>
            )}

            {status === 'error' && (
              <div className="dash-load-error">{error}</div>
            )}
          </div>
        </div>
      </main>
    </div>
  )
}
