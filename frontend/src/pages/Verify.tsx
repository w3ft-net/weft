import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import './Verify.css'

type State = 'verifying' | 'success' | 'error'

export default function Verify() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { verify } = useAuth()
  const [state, setState] = useState<State>('verifying')
  const [errorMsg, setErrorMsg] = useState('')

  useEffect(() => {
    const token = searchParams.get('token')
    if (!token) {
      setState('error')
      setErrorMsg('No token found in URL.')
      return
    }

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
