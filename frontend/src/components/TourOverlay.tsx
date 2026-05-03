import { useEffect } from 'react'
import { useLocation } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { advanceOnboarding } from '../lib/api'
import './TourOverlay.css'

// PanelStep describes one step of the panel-tour stage. Each step
// targets a specific sidebar item; the user advances by clicking the
// pulsing item (which navigates AND auto-advances) or by hitting Next.
//
// The shell-* steps live in a separate map below — their copy comes
// from the server (onboarding_prompt), so the frontend only needs the
// step → highlight-target mapping.
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

// shell-* step → which shell-chrome button to highlight (if any).
// Steps not in this map render the floating overlay only, no
// highlighted button. Hardcoded on the client because the testids
// are UI-element names that already live here.
const SHELL_HIGHLIGHT_TARGETS: Record<string, string> = {
  'shell-cron':           'shell-watches-toggle-button',
  'shell-bin-bash':       'shell-functions-toggle-button',
  'shell-bin-lua':        'shell-functions-toggle-button',
  'shell-notify':         'shell-etc-toggle-button',  // /etc/notify.conf
  'shell-usage':          'shell-etc-toggle-button',  // /etc/usage
  'shell-dashboard-yaml': 'shell-etc-toggle-button',  // /etc/dashboards/*.conf
  'shell-tmp':            'shell-tmp-toggle-button',
}

const REFRESH_INTERVAL_MS = 3000

export default function TourOverlay() {
  const { user, refreshUser } = useAuth()
  const location = useLocation()

  const step = user?.onboarding_step
  const dismissed = user?.onboarding_dismissed
  const inLoom = user?.customer === 'loom'
  const isShell = !!step && step.startsWith('shell-')
  const panel = step ? PANEL_STEPS.find(s => s.step === step) : null

  // Panel-tour auto-advance: navigating to the highlighted route
  // bumps the step server-side; AuthContext picks up the new step
  // on the next refresh.
  useEffect(() => {
    if (!step || dismissed || !inLoom) return
    if (!panel) return
    if (location.pathname === panel.route) {
      advanceOnboarding(step)
        .then(() => refreshUser())
        .catch(e => console.error('panel-tour advance:', e))
    }
  }, [step, dismissed, inLoom, location.pathname, panel, refreshUser])

  // Shell-tour polling: while in shell-* phase, poll /me so the
  // banner updates within a few seconds when the server-side
  // shell-exec hook advances the step. This is the no-SSE path —
  // the SSE event from /api/shell/exec is a low-latency
  // optimization on top of this; if it gets dropped, the poll
  // catches up.
  useEffect(() => {
    if (!isShell || dismissed || !inLoom) return
    const id = setInterval(() => {
      refreshUser().catch(() => {})
    }, REFRESH_INTERVAL_MS)
    return () => clearInterval(id)
  }, [isShell, dismissed, inLoom, refreshUser])

  // Update body[data-tour-highlight] every render so CSS can pulse
  // the right element. Cleanup runs in a separate effect so we
  // don't strand a stale value on unmount.
  useEffect(() => {
    if (!step || dismissed || !inLoom) {
      delete document.body.dataset.tourHighlight
      return
    }
    if (panel) {
      document.body.dataset.tourHighlight = panel.testId
    } else if (isShell && SHELL_HIGHLIGHT_TARGETS[step]) {
      document.body.dataset.tourHighlight = SHELL_HIGHLIGHT_TARGETS[step]
    } else {
      delete document.body.dataset.tourHighlight
    }
  }, [step, dismissed, inLoom, panel, isShell])

  if (!step || dismissed || !inLoom) return null
  if (!panel && !isShell) return null

  // Build the current copy + actions.
  let copy: string
  let progress: string
  let primaryAction: { label: string; testId: string; onClick: () => void } | null = null

  if (panel) {
    copy = panel.copy
    const idx = PANEL_STEPS.findIndex(s => s.step === step)
    progress = `Tour · ${idx + 1} of ${PANEL_STEPS.length}`
    const isLastPanel = idx === PANEL_STEPS.length - 1
    primaryAction = {
      label: isLastPanel ? 'Drop into the shell →' : 'Next →',
      testId: isLastPanel ? 'tour-overlay-shell-button' : 'tour-overlay-next-button',
      onClick: () => {
        advanceOnboarding(step)
          .then(() => {
            // Reload here — panel→shell handoff means the user is
            // about to navigate to /shell where every cached query
            // gets re-issued anyway.
            window.location.reload()
          })
          .catch(e => console.error('panel-tour next:', e))
      },
    }
  } else {
    // Shell phase. Copy comes from the server; no Next button —
    // the user advances by running the pipeline that satisfies the
    // current lesson's predicate.
    copy = user.onboarding_prompt || `Lesson ${step}…`
    progress = 'Tour · shell phase'
  }

  return (
    <div className="tour-overlay" data-testid="tour-overlay">
      <div className="tour-overlay-progress" data-testid="tour-overlay-progress">
        {progress}
      </div>
      <div className="tour-overlay-copy" data-testid="tour-overlay-copy">
        {copy}
      </div>
      {primaryAction && (
        <div className="tour-overlay-actions">
          <button
            className="tour-overlay-button tour-overlay-button-primary"
            onClick={primaryAction.onClick}
            data-testid={primaryAction.testId}
          >
            {primaryAction.label}
          </button>
        </div>
      )}
    </div>
  )
}
