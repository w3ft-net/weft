import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import Pane from '../components/Pane'
import './Login.css'

export default function Login() {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [sent, setSent] = useState(false)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!email.trim()) return
    setError('')
    setLoading(true)
    try {
      await login(email.trim())
      setSent(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="login-page">
      <img src="/weft-logo.svg" alt="weft" className="login-logo" />
      <Pane title="weft login" className="login-card">
        <p className="login-motd">log analysis for your unix heart</p>

        {sent ? (
          <div className="login-sent" data-testid="login-sent-message">
            <div className="login-sent-ok">magic link sent</div>
            <div className="login-sent-line">to: <strong>{email}</strong></div>
            <div className="login-sent-dim">open it to sign in. it expires in 15 minutes.</div>
            <div className="login-sent-dim tui-cursor">waiting</div>
            <button
              className="tui-btn login-resend"
              data-testid="login-resend-button"
              onClick={() => setSent(false)}
            >
              use a different email
            </button>
          </div>
        ) : (
          <form className="login-form" onSubmit={handleSubmit} data-testid="login-form">
            <div className="login-field">
              <label className="login-label" htmlFor="login-email">email</label>
              <input
                id="login-email"
                type="email"
                placeholder="you@company.com"
                data-testid="login-email-input"
                className="login-input"
                value={email}
                onChange={e => setEmail(e.target.value)}
                autoFocus
                required
              />
            </div>
            {error && (
              <div className="login-error" data-testid="login-error-message">{error}</div>
            )}
            <div className="login-actions">
              <button
                type="submit"
                data-testid="login-submit-button"
                className="tui-btn tui-btn-primary login-submit"
                disabled={loading}
              >
                {loading ? 'sending…' : 'send magic link'}
              </button>
            </div>
            <p className="login-help">no password. we email you a one-time link.</p>
          </form>
        )}
      </Pane>

      <div className="login-foot">
        <Link to="/" className="login-back" data-testid="login-back-link">
          ← back to home
        </Link>
      </div>
    </div>
  )
}
