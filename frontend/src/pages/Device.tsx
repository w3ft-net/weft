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
        <div className="dash-content device-content">
          <div className="dash-card">
            <div className="dash-card-header">
              <h2 className="dash-card-title">authorize cli</h2>
            </div>

            {status === 'ready' && (
              <>
                <p className="device-text">
                  The weft CLI is requesting access to your account
                  {user ? ` (${user.email})` : ''}.
                </p>
                <p className="device-text device-text-dim">
                  Enter the code shown in your terminal. Only authorize a code you can see printed
                  by a `weft login` you ran yourself — never one from a link someone sent you.
                </p>
                <input
                  type="text"
                  value={code}
                  onChange={(e) => setCode(e.target.value.toUpperCase())}
                  placeholder="XXXX-XX"
                  autoFocus
                  className="device-code-input"
                  data-testid="device-user-code-input"
                />
                <button
                  className="btn-action btn-action-primary"
                  onClick={handleApprove}
                  disabled={!code.trim()}
                  data-testid="device-approve-button"
                >
                  authorize
                </button>
              </>
            )}

            {status === 'approving' && (
              <div className="dash-empty tui-cursor">authorizing</div>
            )}

            {status === 'done' && (
              <div className="device-done">
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
