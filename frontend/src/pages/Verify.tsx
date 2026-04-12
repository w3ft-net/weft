import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { acceptInvite } from '../lib/api'
import './Verify.css'

type State = 'verifying' | 'success' | 'error' | 'invite-prompt'

export default function Verify() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { verify, loginWithToken } = useAuth()
  const [state, setState] = useState<State>('verifying')
  const [errorMsg, setErrorMsg] = useState('')
  const [accepting, setAccepting] = useState(false)

  const token = searchParams.get('token') || ''
  const isInvite = searchParams.get('invite') === '1'

  useEffect(() => {
    if (!token) {
      setState('error')
      setErrorMsg('No token found in URL.')
      return
    }

    if (isInvite) {
      // Show the accept prompt instead of auto-verifying.
      setState('invite-prompt')
      return
    }

    // Normal login flow.
    verify(token)
      .then(() => {
        setState('success')
        setTimeout(() => navigate('/dashboard'), 1200)
      })
      .catch(err => {
        setState('error')
        setErrorMsg(err instanceof Error ? err.message : 'Verification failed.')
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function handleAcceptInvite() {
    setAccepting(true)
    try {
      const result = await acceptInvite(token)
      // Store the JWT and load user info.
      if (result.token) {
        await loginWithToken(result.token)
      }
      setState('success')
      setTimeout(() => navigate('/dashboard'), 1200)
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
