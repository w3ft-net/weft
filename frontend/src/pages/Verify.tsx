import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { acceptInvite } from '../lib/api'
import Pane from '../components/Pane'
import './Login.css'
import './Verify.css'

// Magic-link tokens are single-use. React.StrictMode runs the mount effect
// twice in dev, which would send two verifies — the second 401s on the
// already-consumed token. Share one in-flight call per token across runs.
const inflightVerify = new Map<string, Promise<{ signupTier?: string }>>()

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
      setState('invite-prompt')
      return
    }

    // Login or signup link.
    let call = inflightVerify.get(token)
    if (!call) {
      call = verify(token)
      inflightVerify.set(token, call)
    }
    call
      .then(resp => {
        setState('success')
        // A finished signup carries on to the payment step it was headed for.
        const next = resp.signupTier
          ? `/signup/payment?tier=${encodeURIComponent(resp.signupTier)}`
          : '/dashboards'
        setTimeout(() => navigate(next), 1200)
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
    <div className="login-page">
      <img src="/weft-logo.svg" alt="weft" className="login-logo" />
      <Pane title="weft verify" className="login-card">
        {state === 'verifying' && (
          <div className="verify-body" data-testid="verify-verifying-state">
            <p className="verify-status">
              <span className="verify-spinner" aria-hidden="true" />verifying magic link…
            </p>
          </div>
        )}

        {state === 'invite-prompt' && (
          <div className="verify-body" data-testid="verify-invite-state">
            <p className="verify-status">you've been invited to join a team on weft.</p>
            <button
              className="tui-btn tui-btn-primary verify-accept-btn"
              onClick={handleAcceptInvite}
              disabled={accepting}
              data-testid="verify-accept-invite-button"
            >
              {accepting ? 'joining…' : 'accept invite'}
            </button>
          </div>
        )}

        {state === 'success' && (
          <div className="verify-body" data-testid="verify-success-state">
            <p className="verify-status verify-status-ok">verified</p>
            <p className="verify-hint tui-cursor">redirecting</p>
          </div>
        )}

        {state === 'error' && (
          <div className="verify-body" data-testid="verify-error-state">
            <p className="verify-status verify-status-err">{errorMsg}</p>
            <p className="verify-hint">magic links expire after 15 minutes.</p>
            <Link
              to="/login"
              className="tui-btn verify-back"
              data-testid="verify-login-link"
            >
              request a new link
            </Link>
          </div>
        )}
      </Pane>
    </div>
  )
}
