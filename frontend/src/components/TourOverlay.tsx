import { useEffect } from 'react'
import { useLocation } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { advanceOnboarding, getMe } from '../lib/api'
import './TourOverlay.css'

// PanelStep describes one step of the panel-tour stage. Each step
// targets a specific sidebar item; the user advances by clicking the
// pulsing item (which navigates AND auto-advances) or by hitting Next.
//
// The shell-* steps are NOT in this list — they're handled by a
// shell-side overlay later in the flow.
interface PanelStep {
  step: string
  route: string
  testId: string         // sidebar item's data-testid for visual targeting
  copy: string
}

const PANEL_STEPS: PanelStep[] = [
  {
    step: 'dashboards',
    route: '/dashboards',
    testId: 'sidebar-dashboards-link',
    copy:
      "Dashboards. Live charts of every metric Loom emits — error rates, " +
      "latency, throughput. The pulsing item is what we'll explore now; " +
      "click it to dive in, or hit Next.",
  },
  {
    step: 'namespaces',
    route: '/namespaces',
    testId: 'sidebar-namespaces-link',
    copy:
      "Namespaces. Each app's logs live in its own namespace, with its " +
      "own access controls and retention. This is where you'd configure " +
      "ingestion when you're ready to ship your own logs.",
  },
  {
    step: 'downloads',
    route: '/downloads',
    testId: 'sidebar-downloads-link',
    copy:
      "Downloads. Grab the weft CLI and the weftd agent — that's how " +
      "you'll stream logs into your own org from your servers and " +
      "containers. After this, we'll drop you into the shell.",
  },
]

const SHELL_HANDOFF_STEP = 'shell-ls'

export default function TourOverlay() {
  const { user } = useAuth()
  const location = useLocation()

  const step = user?.onboarding_step
  const dismissed = user?.onboarding_dismissed
  const inLoom = user?.customer === 'loom'

  // Auto-advance: when the user navigates to the route the current
  // step targets, the server CASes onboarding_step forward. The next
  // /me poll refreshes the User and the overlay slides to the next
  // step (or unmounts if the tour ended).
  useEffect(() => {
    if (!step || dismissed || !inLoom) return
    const current = PANEL_STEPS.find(s => s.step === step)
    if (!current) return
    if (location.pathname === current.route) {
      advanceOnboarding(step)
        .then(() => getMe())
        .catch(e => console.error('panel-tour advance:', e))
    }
  }, [step, dismissed, inLoom, location.pathname])

  if (!step || dismissed || !inLoom) return null
  const active = PANEL_STEPS.find(s => s.step === step)
  if (!active) return null

  // Annotate the body with a data attribute the sidebar's CSS uses to
  // pulse the matching item. Single source of truth: this component
  // owns "what's the active step" and the sidebar reacts purely via
  // CSS on the data-tour-highlight attribute.
  document.body.dataset.tourHighlight = active.testId

  async function handleNext() {
    if (!active) return
    try {
      await advanceOnboarding(active.step)
      // Re-fetch /me so the overlay flips to the next step or unmounts.
      // AuthContext currently doesn't expose a refresh method; reload
      // is the simplest correct path. (Pages cached against the old
      // step would otherwise still show stale highlight.)
      window.location.reload()
    } catch (e) {
      console.error('panel-tour next:', e)
    }
  }

  // Where in the flow are we? "step 2 of 3" etc.
  const idx = PANEL_STEPS.findIndex(s => s.step === step)
  const progress = `${idx + 1} of ${PANEL_STEPS.length}`

  // After downloads, the next step is the shell handoff — show a
  // distinct "Drop into the shell" CTA instead of plain Next.
  const isLastPanel = active.step === PANEL_STEPS[PANEL_STEPS.length - 1].step

  return (
    <div className="tour-overlay" data-testid="tour-overlay">
      <div className="tour-overlay-progress" data-testid="tour-overlay-progress">
        Tour · {progress}
      </div>
      <div className="tour-overlay-copy" data-testid="tour-overlay-copy">
        {active.copy}
      </div>
      <div className="tour-overlay-actions">
        {isLastPanel ? (
          <button
            className="tour-overlay-button tour-overlay-button-primary"
            onClick={handleNext}
            data-testid="tour-overlay-shell-button"
          >
            Drop into the shell →
          </button>
        ) : (
          <button
            className="tour-overlay-button tour-overlay-button-primary"
            onClick={handleNext}
            data-testid="tour-overlay-next-button"
          >
            Next →
          </button>
        )}
      </div>
    </div>
  )
}

// Helper for shell-* steps — exported so the Shell page can import it
// and decide whether to mount the in-shell tour banner. Placed here
// so the canonical "is this step a shell step?" predicate lives next
// to PANEL_STEPS rather than being duplicated.
export function isShellStep(step: string | undefined): boolean {
  return !!step && step.startsWith('shell-') && step !== SHELL_HANDOFF_STEP.replace('shell-', '')
}
