import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
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
      <div className="login-card">
        <div className="login-logo">weft</div>
        <div className="login-tagline">log analysis for your Unix heart</div>

        {sent ? (
          <div className="login-sent" data-testid="login-sent-message">
            <div className="login-sent-icon">✓</div>
            <h2>Check your email</h2>
            <p>
              We sent a magic link to <strong>{email}</strong>.
              Click it to sign in — it expires in 15 minutes.
            </p>
            <button
              className="login-resend"
              data-testid="login-resend-button"
              onClick={() => setSent(false)}
            >
              ← Try a different email
            </button>
          </div>
        ) : (
          <form className="login-form" onSubmit={handleSubmit} data-testid="login-form">
            <label className="login-label" htmlFor="login-email">Email address</label>
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
            {error && (
              <div className="login-error" data-testid="login-error-message">{error}</div>
            )}
            <button
              type="submit"
              data-testid="login-submit-button"
              className="login-submit"
              disabled={loading}
            >
              {loading ? 'Sending...' : 'Send magic link'}
            </button>
          </form>
        )}

        <Link to="/" className="login-back" data-testid="login-back-link">
          ← Back to home
        </Link>
      </div>
    </div>
  )
}
