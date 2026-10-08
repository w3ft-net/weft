import { useState, type FormEvent } from 'react'
import { Link, useSearchParams, useNavigate } from 'react-router-dom'
import { signup } from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import Pane from '../components/Pane'
import './Login.css'
import './Signup.css'

const TIER_LABELS: Record<string, string> = {
  swatch: 'swatch  $19/mo',
  yard: 'yard  $79/mo',
  bolt: 'bolt  $299/mo',
}

export default function Signup() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { loginWithToken } = useAuth()

  const tierParam = searchParams.get('tier') || 'swatch'
  const tier = tierParam in TIER_LABELS ? tierParam : 'swatch'

  const [email, setEmail] = useState('')
  const [orgName, setOrgName] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!email.trim() || !orgName.trim()) return
    setError('')
    setLoading(true)
    try {
      const resp = await signup(email.trim(), orgName.trim(), tier)
      await loginWithToken(resp.token)
      navigate(`/signup/payment?tier=${encodeURIComponent(resp.tier)}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Something went wrong.')
      setLoading(false)
    }
  }

  return (
    <div className="login-page">
      <img src="/weft-logo.svg" alt="weft" className="login-logo" />
      <Pane title="create account" meta="step 1/2" className="login-card">
        <p className="login-motd">log analysis for your unix heart</p>

        <form className="login-form" onSubmit={handleSubmit} data-testid="signup-form">
          <div className="login-field">
            <label className="login-label" htmlFor="signup-email">email</label>
            <input
              id="signup-email"
              type="email"
              placeholder="you@company.com"
              data-testid="signup-email-input"
              className="login-input"
              value={email}
              onChange={e => setEmail(e.target.value)}
              autoFocus
              required
            />
          </div>

          <div className="login-field">
            <label className="login-label" htmlFor="signup-org">org</label>
            <input
              id="signup-org"
              type="text"
              placeholder="acme"
              data-testid="signup-org-input"
              className="login-input"
              value={orgName}
              onChange={e => setOrgName(e.target.value)}
              required
            />
          </div>

          <div className="signup-tier" data-testid="signup-tier-display">
            <span className="signup-tier-label">plan</span>
            <span className="signup-tier-value">{TIER_LABELS[tier]}</span>
            <a href="/#pricing" className="signup-tier-change" data-testid="signup-change-tier-link">change</a>
          </div>

          {error && (
            <div className="login-error" data-testid="signup-error-message">
              {error}
              {error === 'account already exists' && (
                <> — <Link to="/login" data-testid="signup-login-link">log in instead</Link></>
              )}
            </div>
          )}

          <div className="login-actions">
            <button
              type="submit"
              data-testid="signup-submit-button"
              className="tui-btn tui-btn-primary login-submit"
              disabled={loading}
            >
              {loading ? 'creating account…' : 'start free trial'}
            </button>
          </div>

          <p className="signup-disclaimer">
            no charge until your 14-day trial ends. cancel anytime.
          </p>
        </form>
      </Pane>

      <div className="login-foot">
        <span className="signup-login-prompt">
          have an account? <Link to="/login" data-testid="signup-existing-login-link">log in</Link>
        </span>
        <Link to="/" className="login-back" data-testid="signup-back-link">
          ← back to home
        </Link>
      </div>
    </div>
  )
}
