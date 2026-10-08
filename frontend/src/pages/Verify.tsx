import { useEffect, useState } from 'react'
import { useSearchParams, useNavigate, Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { acceptInvite } from '../lib/api'
import Pane from '../components/Pane'
import './Login.css'
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

        {state === 'needs-org' && (
          <div className="verify-body" data-testid="verify-org-state">
            <p className="verify-status">welcome{email ? `, ${email}` : ''}.</p>
            <p className="verify-hint">choose a name for your organization.</p>
            <form className="login-form" onSubmit={handleCreateOrg}>
              <div className="login-field">
                <label className="login-label" htmlFor="verify-org">org</label>
                <input
                  id="verify-org"
                  className="login-input verify-org-input"
                  type="text"
                  placeholder="acme"
                  value={orgName}
                  onChange={e => setOrgName(e.target.value)}
                  autoFocus
                  required
                  data-testid="verify-org-name-input"
                />
              </div>
              <div className="login-actions">
                <button
                  type="submit"
                  className="tui-btn tui-btn-primary verify-accept-btn"
                  data-testid="verify-create-org-button"
                >
                  create account
                </button>
              </div>
            </form>
          </div>
        )}

        {state === 'creating' && (
          <div className="verify-body" data-testid="verify-creating-state">
            <p className="verify-status">
              <span className="verify-spinner" aria-hidden="true" />creating your account…
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
