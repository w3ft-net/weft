import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { acceptInvite } from '../lib/api'
import './Verify.css'

type State = 'verifying' | 'needs-org' | 'creating' | 'success' | 'error' | 'invite-prompt'

export default function Verify() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { verify, loginWithToken } = useAuth()
  const [state, setState] = useState<State>('verifying')
  const [errorMsg, setErrorMsg] = useState('')
  const [accepting, setAccepting] = useState(false)
  const [orgName, setOrgName] = useState('')
  const [email, setEmail] = useState('')

  const token = searchParams.get('token') || ''
  const isInvite = searchParams.get('invite') === '1'

  useEffect(() => {
    if (!token) {
      setState('error')
      setErrorMsg('No token found in URL.')
      return
    }

    if (isInvite) {
      setState('invite-prompt')
      return
    }

    // Normal login flow.
    verify(token)
      .then(resp => {
        if (resp.needs_org) {
          setEmail(resp.email || '')
          setState('needs-org')
        } else {
          setState('success')
          setTimeout(() => navigate('/dashboards'), 1200)
        }
      })
      .catch(err => {
        setState('error')
        setErrorMsg(err instanceof Error ? err.message : 'Verification failed.')
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleCreateOrg(e: React.FormEvent) {
    e.preventDefault()
    if (!orgName.trim()) return
    setState('creating')
    try {
      const resp = await verify(token, orgName.trim())
      if (resp.needs_org) {
        setState('needs-org')
        return
      }
      setState('success')
      setTimeout(() => navigate('/dashboards'), 1200)
    } catch (err) {
      setState('error')
      setErrorMsg(err instanceof Error ? err.message : 'Failed to create account.')
    }
  }

  async function handleAcceptInvite() {
    setAccepting(true)
    try {
      const result = await acceptInvite(token)
      if (result.token) {
        await loginWithToken(result.token)
      }
      setState('success')
      setTimeout(() => navigate('/dashboards'), 1200)
    } catch (err) {
      setState('error')
      setErrorMsg(err instanceof Error ? err.message : 'Failed to accept invite.')
    } finally {
      setAccepting(false)
    }
  }

  return (
    <div className="verify-page">
      <div className="verify-card">
        <div className="verify-logo">weft</div>

        {state === 'verifying' && (
          <div className="verify-body" data-testid="verify-verifying-state">
            <div className="verify-spinner" />
            <p className="verify-status">Verifying magic link...</p>
          </div>
        )}

        {state === 'needs-org' && (
          <div className="verify-body" data-testid="verify-org-state">
            <p className="verify-status">Welcome{email ? `, ${email}` : ''}!</p>
            <p className="verify-hint" style={{ marginBottom: 16 }}>Choose a name for your organization.</p>
            <form onSubmit={handleCreateOrg}>
              <input
                className="verify-org-input"
                type="text"
                placeholder="e.g., kevlar, acme"
                value={orgName}
                onChange={e => setOrgName(e.target.value)}
                autoFocus
                required
                data-testid="verify-org-name-input"
                style={{
                  width: '100%', padding: '10px 14px', marginBottom: 12,
                  background: 'var(--bg-surface, #1a1d26)', border: '1px solid var(--border, #2d3345)',
                  borderRadius: 4, color: 'var(--text-primary, #c8ccd4)',
                  fontFamily: '"JetBrains Mono", monospace', fontSize: '0.9rem',
                }}
              />
              <button
                type="submit"
                className="verify-accept-btn"
                data-testid="verify-create-org-button"
              >
                Create account
              </button>
            </form>
          </div>
        )}

        {state === 'creating' && (
          <div className="verify-body" data-testid="verify-creating-state">
            <div className="verify-spinner" />
            <p className="verify-status">Creating your account...</p>
          </div>
        )}

        {state === 'invite-prompt' && (
          <div className="verify-body" data-testid="verify-invite-state">
            <p className="verify-status">You've been invited to join a team on Weft.</p>
            <button
              className="verify-accept-btn"
              onClick={handleAcceptInvite}
              disabled={accepting}
              data-testid="verify-accept-invite-button"
            >
              {accepting ? 'Joining...' : 'Accept Invite'}
            </button>
          </div>
        )}

        {state === 'success' && (
          <div className="verify-body" data-testid="verify-success-state">
            <div className="verify-success-icon">✓</div>
            <p className="verify-status verify-status-ok">Verified! Redirecting...</p>
          </div>
        )}

        {state === 'error' && (
          <div className="verify-body" data-testid="verify-error-state">
            <div className="verify-error-icon">✗</div>
            <p className="verify-status verify-status-err">{errorMsg}</p>
            <p className="verify-hint">Magic links expire after 15 minutes.</p>
            <Link
              to="/login"
              className="verify-back"
              data-testid="verify-login-link"
            >
              ← Request a new link
            </Link>
          </div>
        )}
      </div>
    </div>
  )
}
