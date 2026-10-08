import { useState } from 'react'
import { resendVerification } from '../lib/api'
import { useAuth } from '../lib/AuthContext'
import './VerificationBanner.css'

// VerificationBanner appears at the top of authenticated pages when the
// signed-in user hasn't proven email ownership yet. State-changing API
// calls (token creation, integrations, source registration, team invites)
// will return 403 with code=email_verification_required until they do,
// so the banner's job is to push them toward the "check your email" click.
//
// The server sweeps unverified accounts after UnverifiedGracePeriod; the
// countdown shown here comes from user.verification_deadline.
export default function VerificationBanner() {
  const { user } = useAuth()
  const [status, setStatus] = useState<'idle' | 'sending' | 'sent' | 'error'>('idle')
  const [errMsg, setErrMsg] = useState('')

  if (!user || user.email_verified !== false) return null

  async function handleResend() {
    setStatus('sending')
    setErrMsg('')
    try {
      await resendVerification()
      setStatus('sent')
    } catch (e) {
      setStatus('error')
      setErrMsg(e instanceof Error ? e.message : 'Failed to resend.')
    }
  }

  const deadline = user.verification_deadline
  const hoursLeft = deadline ? Math.max(0, Math.round((deadline * 1000 - Date.now()) / 3_600_000)) : null

  return (
    <div className="verify-banner" data-testid="verification-banner">
      <div className="verify-banner-msg">
        <span className="verify-banner-sigil" aria-hidden="true">!</span>
        <strong>verify your email</strong> to unlock agent tokens, crons, and integrations.
        {hoursLeft !== null && hoursLeft > 0 && (
          <> the link expires in <strong>{hoursLeft}h</strong>.</>
        )}
      </div>
      <div className="verify-banner-actions">
        {status === 'idle' && (
          <button
            className="verify-banner-btn tui-btn"
            onClick={handleResend}
            data-testid="verification-banner-resend-button"
          >
            resend email
          </button>
        )}
        {status === 'sending' && <span className="verify-banner-status tui-cursor">sending</span>}
        {status === 'sent' && (
          <span className="verify-banner-status verify-banner-status-ok" data-testid="verification-banner-sent">
            sent. check your inbox.
          </span>
        )}
        {status === 'error' && (
          <span className="verify-banner-status verify-banner-status-err" data-testid="verification-banner-error">
            {errMsg || "couldn't resend the email. try again."}
          </span>
        )}
      </div>
    </div>
  )
}
