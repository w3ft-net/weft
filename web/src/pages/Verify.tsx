import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useExtension } from '../extend'
import { useAuth } from '../lib/AuthContext'
import Pane from '../components/Pane'
import './Login.css'
import './Verify.css'

// Magic-link tokens are single-use. React.StrictMode runs the mount effect
// twice in dev, which would send two verifies — the second 401s on the
// already-consumed token. Share one in-flight call per token across runs.
const inflightVerify = new Map<string, Promise<{ signupTier?: string }>>()

type State = 'verifying' | 'success' | 'error'

export default function Verify() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { verify } = useAuth()
  const ext = useExtension()
  const [state, setState] = useState<State>('verifying')
  const [errorMsg, setErrorMsg] = useState('')

  const token = searchParams.get('token') || ''

  useEffect(() => {
    if (!token) {
      setState('error')
      setErrorMsg('No token found in URL.')
      return
    }

    // A sign-in link.
    let call = inflightVerify.get(token)
    if (!call) {
      call = verify(token)
      inflightVerify.set(token, call)
    }
    call
      .then(() => {
        setState('success')
        setTimeout(() => navigate(ext.homePath ?? '/dashboards'), 1200)
      })
      .catch(err => {
        setState('error')
        setErrorMsg(err instanceof Error ? err.message : 'Verification failed.')
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

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
              to={ext.loginPath ?? '/login'}
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
