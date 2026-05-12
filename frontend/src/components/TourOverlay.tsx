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
    step: 'downloads',
    route: '/downloads',
    testId: 'sidebar-downloads-link',
    copy:
      "You'll want to grab the weft CLI, and — if you have deployments where you run it — weftd, our logging agent — to start shipping those log lines.",
  },
  {
    step: 'namespaces',
    route: '/namespaces',
    testId: 'sidebar-namespaces-link',
    copy:
      "Namespaces are containers for log integrations, whether via weftd, CloudWatch, or Heroku. You can slice up your log producers however you want, but we tend to think of namespaces as an application, and then the environments and services in that application get assigned tokens within that namespace. You can create namespaces here in the browser or using the weft cli. Note that Loom Inc has only one namespace (loom). We'll see that again in a bit",
  },
  {
    step: 'dashboards',
    route: '/dashboards',
    testId: 'sidebar-dashboards-link',
    copy:
      "Dashboards can be viewed here in the browser or in the terminal. Dashboards are defined in YAML and are built on top of timeseries data you create with crons operating on your logs or metrics data from weftd — all of which happens in the weft shell.",
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
  'shell-notify':         'shell-etc-toggle-button',  // /etc/notify.yaml
  'shell-usage':          'shell-etc-toggle-button',  // /etc/usage
  'shell-dashboard-yaml': 'shell-etc-toggle-button',  // /etc/dashboards/*.yaml
  'shell-tmp':            'shell-tmp-toggle-button',
}

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

  // Shell-tour advancement: listen for the tour-advance DOM event
  // fired by the SSE parser when the server advances the step, AND
  // by notifyBuiltinExecuted when a browser-side builtin (ls, cat
  // /etc/…) finishes — those never round-trip through SSE, so the
  // event from network.ts is the only signal we get for them.
  useEffect(() => {
    if (!isShell || dismissed || !inLoom) return
    const onAdvance = () => { refreshUser().catch(() => {}) }
    window.addEventListener('tour-advance', onAdvance)
    return () => {
      window.removeEventListener('tour-advance', onAdvance)
    }
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
    // Shell phase. Copy comes from the server; the user advances by
    // running the lesson's Action verbatim (we pre-inject it into the
    // input).
    copy = user.onboarding_prompt || `Lesson ${step}…`
    progress = 'Tour · shell phase'
  }

  const shellAction = isShell ? user.onboarding_action : undefined

  return (
    <div className="tour-overlay" data-testid="tour-overlay">
      <div className="tour-overlay-progress" data-testid="tour-overlay-progress">
        {progress}
      </div>
      <div className="tour-overlay-copy" data-testid="tour-overlay-copy">
        {copy}
      </div>
      {shellAction && (
        <button
          type="button"
          className="tour-overlay-paste"
          title={`Re-paste: ${shellAction}`}
          aria-label="Re-paste suggested command"
          data-testid="tour-overlay-paste-button"
          onClick={() => {
            window.dispatchEvent(new CustomEvent('tour-action-paste'))
          }}
        >
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
            <path
              fill="none"
              stroke="currentColor"
              strokeWidth="1.4"
              strokeLinecap="round"
              strokeLinejoin="round"
              d="M5 3.5h6M5 3.5a1 1 0 0 0-1 1V13a1 1 0 0 0 1 1h6a1 1 0 0 0 1-1V4.5a1 1 0 0 0-1-1M6.5 2.5h3a1 1 0 0 1 1 1v.5h-5v-.5a1 1 0 0 1 1-1Z"
            />
          </svg>
          <code className="tour-overlay-paste-text">{shellAction}</code>
        </button>
      )}
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
