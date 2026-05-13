import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { dismissGraduationNotice } from '../lib/api'
import './GraduationBanner.css'

// GraduationBanner is the one-shot success banner that renders on the
// user's home org right after they finish the Loom tour. The server
// gates this on (onboarding_step === 'done' && !HasSeenNotice); the
// dismiss endpoint flips the notice so the banner doesn't re-appear.
export default function GraduationBanner() {
  const { user, refreshUser } = useAuth()
  const [dismissing, setDismissing] = useState(false)

  if (!user) return null
  if (!user.pending_graduation_notice) return null

  async function handleDismiss() {
    setDismissing(true)
    try {
      await dismissGraduationNotice()
      await refreshUser()
    } catch (e) {
      console.error('dismiss graduation notice:', e)
      setDismissing(false)
    }
  }

  return (
    <div className="graduation-banner" data-testid="graduation-banner">
      <div className="graduation-banner-body">
        <div className="graduation-banner-title">You've graduated.</div>
        <div className="graduation-banner-copy">
          Loom Inc has been removed from your orgs. Time to set up your own
          namespaces and start streaming logs — head to{' '}
          <Link to="/namespaces" data-testid="graduation-banner-namespaces-link">
            Namespaces
          </Link>{' '}
          to create your first one, then grab the agent on the{' '}
          <Link to="/downloads" data-testid="graduation-banner-downloads-link">
            Downloads
          </Link>{' '}
          page. Everything else is in the{' '}
          <Link to="/manual" data-testid="graduation-banner-manual-link">
            manual
          </Link>
          .
        </div>
      </div>
      <button
        type="button"
        className="graduation-banner-dismiss"
        onClick={handleDismiss}
        disabled={dismissing}
        data-testid="graduation-banner-dismiss-button"
        aria-label="Dismiss"
      >
        ×
      </button>
    </div>
  )
}
