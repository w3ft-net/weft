import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { apiFetch } from '../lib/api'
import './Dashboard.css'

export default function Device() {
  const [searchParams] = useSearchParams()
  const { user } = useAuth()
  const code = searchParams.get('code') || ''
  const [status, setStatus] = useState<'ready' | 'approving' | 'done' | 'error'>('ready')
  const [error, setError] = useState('')

  async function handleApprove() {
    setStatus('approving')
    try {
      await apiFetch('/api/auth/device/approve', {
        method: 'POST',
        body: JSON.stringify({ user_code: code }),
      })
      setStatus('done')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to approve')
      setStatus('error')
    }
  }

  if (!code) {
    return (
      <div className="app-layout">
        <main className="app-main" data-testid="device-main">
          <div className="dash-content" style={{ maxWidth: 480, paddingTop: 80 }}>
            <div className="dash-card">
              <div className="dash-card-header">
                <h2 className="dash-card-title">CLI Authorization</h2>
              </div>
              <div className="dash-empty">No device code provided.</div>
            </div>
          </div>
        </main>
      </div>
    )
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
                  Verify this code matches what the CLI is showing:
                </p>
                <div style={{
                  fontFamily: 'var(--font-mono)', fontSize: '2rem', fontWeight: 700,
                  color: 'var(--accent)', textAlign: 'center', padding: '16px 0', marginBottom: 20,
                  letterSpacing: '0.1em',
                }} data-testid="device-user-code">
                  {code}
                </div>
                <button
                  className="btn-action btn-action-primary"
                  style={{ width: '100%', padding: '12px' }}
                  onClick={handleApprove}
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
