import { useAuth } from '../lib/AuthContext'
import { advanceOnboarding, dismissOnboarding } from '../lib/api'
import Pane from './Pane'
import './WelcomeModal.css'

// WelcomeModal renders for fresh signups whose onboarding_step is
// still 'welcome' and who haven't dismissed. "Start the tour" advances
// to the dashboards step and switches the active org to Loom; "Skip"
// dismisses permanently. The user keeps Loom membership in either
// case — a skipper can still browse the demo via the org dropdown.
//
// Hangs off AuthProvider so it renders above any page; mounts/un-
// mounts based on the user's onboarding state and the orgs list
// (we need Loom in the list before we can switch into it).
export default function WelcomeModal() {
  const { user, switchOrg } = useAuth()

  if (!user) return null
  if (user.onboarding_step !== 'welcome') return null
  if (user.onboarding_dismissed) return null

  const hasLoom = user.orgs?.some(o => o.name === 'loom')

  async function handleStart() {
    try {
      await advanceOnboarding('welcome')
    } catch (e) {
      // Best effort — if the advance fails, switching orgs would
      // still trigger the tour overlay on the next /me poll. Log
      // and proceed.
      console.error('advance welcome:', e)
    }
    if (hasLoom) {
      switchOrg('loom', '/downloads') // the tour's first page; its overlay takes over
    }
  }

  async function handleSkip() {
    try {
      await dismissOnboarding()
    } catch (e) {
      console.error('dismiss onboarding:', e)
    }
    // No reload — AuthContext's user state will be refreshed on the
    // next /me call; the modal hides as soon as React re-renders
    // with onboarding_dismissed=true. To avoid waiting for that,
    // poke the local state by reloading.
    window.location.reload()
  }

  return (
    <div className="welcome-modal-backdrop" data-testid="welcome-modal-backdrop">
      <Pane
        className="welcome-modal"
        title="welcome to weft"
        role="dialog"
        aria-modal="true"
        aria-label="welcome to weft"
        data-testid="welcome-modal"
      >
        <p className="welcome-modal-body">
          To help you learn weft at warp-speed, we've made you a member of{' '}
          <strong>Loom Inc</strong>{hasLoom ? '' : ' (still warming up — give it a moment)'} —
          a fake org with realistic logs flowing through it. Take the tour and
          we'll walk you through every part of weft using Loom's data.
        </p>
        <p className="welcome-modal-body welcome-modal-body-quiet">
          You can switch back to your own org any time using the org menu
          in the sidebar.
        </p>
        <div className="welcome-modal-actions">
          <button
            className="tui-btn"
            onClick={handleSkip}
            data-testid="welcome-modal-skip-button"
          >
            skip
          </button>
          <button
            className="tui-btn tui-btn-primary"
            onClick={handleStart}
            disabled={!hasLoom}
            data-testid="welcome-modal-start-button"
            autoFocus
          >
            start the tour
          </button>
        </div>
      </Pane>
    </div>
  )
}
