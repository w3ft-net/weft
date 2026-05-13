import { useState, type FormEvent } from 'react'
import { Link, useSearchParams, useNavigate } from 'react-router-dom'
import { signup } from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import './Signup.css'

const TIER_LABELS: Record<string, string> = {
  swatch: 'Swatch — $19/mo',
  yard: 'Yard — $79/mo',
  bolt: 'Bolt — $299/mo',
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
      <div className="login-card">
        <div className="login-logo">weft</div>
        <div className="login-tagline">log analysis for your Unix heart</div>

        <form className="login-form" onSubmit={handleSubmit} data-testid="signup-form">
          <label className="login-label" htmlFor="signup-email">Email address</label>
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

          <label className="login-label" htmlFor="signup-org">Organization name</label>
          <input
            id="signup-org"
            type="text"
            placeholder="e.g., acme"
            data-testid="signup-org-input"
            className="login-input"
            value={orgName}
            onChange={e => setOrgName(e.target.value)}
            required
          />

          <div className="signup-tier" data-testid="signup-tier-display">
            <span className="signup-tier-label">Plan</span>
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

          <button
            type="submit"
            data-testid="signup-submit-button"
            className="login-submit"
            disabled={loading}
          >
            {loading ? 'Creating account...' : 'Start free trial'}
          </button>

          <p className="signup-disclaimer">
            Your card won't be charged until your 14-day trial ends. Cancel anytime.
          </p>
        </form>

        <div className="signup-login-prompt">
          Already have an account? <Link to="/login" data-testid="signup-existing-login-link">Log in</Link>
        </div>

        <Link to="/" className="login-back" data-testid="signup-back-link">
          ← Back to home
        </Link>
      </div>
    </div>
  )
}
