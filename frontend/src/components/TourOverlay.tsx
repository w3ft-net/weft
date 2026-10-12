import { useEffect, Fragment, type ReactNode } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../lib/AuthContext'
import { advanceOnboarding } from '../lib/api'
import './TourOverlay.css'

// renderPromptCopy turns a lesson Prompt string into React nodes, with
// `backtick`-wrapped spans becoming <code> and \n\n splitting into
// paragraphs. Lesson copy in coordinator/onboarding/lessons.go only
// uses those two features — keeping this inline avoids pulling in a
// full markdown dep just for the overlay.
function renderPromptCopy(text: string): ReactNode {
  const paragraphs = text.split(/\n\n+/)
  return paragraphs.map((p, pi) => (
    <p key={pi} className="tour-overlay-copy-paragraph">
      {renderInlineBackticks(p)}
    </p>
  ))
}

function renderInlineBackticks(text: string): ReactNode[] {
  const parts = text.split(/(`[^`]+`)/g)
  return parts.map((part, i) => {
    if (part.startsWith('`') && part.endsWith('`') && part.length >= 2) {
      return <code key={i}>{part.slice(1, -1)}</code>
    }
    return <Fragment key={i}>{part}</Fragment>
  })
}

// PanelStep describes one step of the panel-tour stage. The copy
// describes the page at `route`, so the overlay only shows it once the
// user is on that page; elsewhere it offers a button that goes there.
// Next advances the step and lands on the following page.
//
// The shell-* steps live in a separate map below — their copy comes
// from the server (onboarding_prompt), so the frontend only needs the
// step → highlight-target mapping.
interface PanelStep {
  step: string
  route: string
  label: string          // what the page is called, for "show me the …"
  copy: string
}

// GRADUATE_STEP follows the last shell lesson. Finishing the lesson
// must not eject the user mid-shell, so the tour parks here until they
// choose to leave.
const GRADUATE_STEP = 'graduate'
const GRADUATE_COPY =
  "That's the tour. You've got the shell, the pseudofiles, bins, crons, dashboards and git history of your own org's config. When you finish, you'll leave Loom and land in your own org, where you can set up your first namespace. You can come back to the shell any time before then."

const PANEL_STEPS: PanelStep[] = [
  {
    step: 'downloads',
    route: '/downloads',
    label: 'downloads',
    copy:
      "You'll want to grab the weft CLI, and — if you have deployments where you run it — weftd, our logging agent — to start shipping those log lines.",
  },
  {
    step: 'namespaces',
    route: '/namespaces',
    label: 'namespaces',
    copy:
      "Namespaces are containers for log integrations, whether via weftd, CloudWatch, or Heroku. You can slice up your log producers however you want, but we tend to think of namespaces as an application, and then the environments and services in that application get assigned tokens within that namespace. You can create namespaces here in the browser or using the weft cli. Note that Loom Inc has only one namespace (loom). We'll see that again in a bit",
  },
  {
    step: 'dashboards',
    route: '/dashboards',
    label: 'dashboards',
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
  const navigate = useNavigate()

  const step = user?.onboarding_step
  const dismissed = user?.onboarding_dismissed
  const inLoom = user?.customer === 'loom'
  const isShell = !!step && step.startsWith('shell-')
  const panel = step ? PANEL_STEPS.find(s => s.step === step) : null
  const isGraduate = step === GRADUATE_STEP

  // Shell-tour advancement: listen for the tour-advance DOM event
  // fired by the SSE parser when the server advances the step, AND
  // by notifyBuiltinExecuted when a browser-side builtin (ls, cat
  // /etc/…) finishes — those never round-trip through SSE, so the
  // event from network.ts is the only signal we get for them.
  useEffect(() => {
    if (!isShell || dismissed || !inLoom) return
    const onAdvance = () => {
      refreshUser().catch(() => {})
    }
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
    if (isShell && SHELL_HIGHLIGHT_TARGETS[step]) {
      document.body.dataset.tourHighlight = SHELL_HIGHLIGHT_TARGETS[step]
    } else {
      delete document.body.dataset.tourHighlight
    }
  }, [step, dismissed, inLoom, isShell])

  if (!step || dismissed || !inLoom) return null
  if (!panel && !isShell && !isGraduate) return null

  // Build the current copy + actions.
  let copy: string
  let progress: string
  let primaryAction: { label: string; testId: string; onClick: () => void } | null = null

  if (panel) {
    const idx = PANEL_STEPS.findIndex(s => s.step === step)
    progress = `tour ${idx + 1}/${PANEL_STEPS.length}`
    if (location.pathname !== panel.route) {
      copy = `Click “show me ${panel.label}” to get started.`
      primaryAction = {
        label: `show me ${panel.label}`,
        testId: 'tour-overlay-start-button',
        onClick: () => navigate(panel.route),
      }
    } else {
      copy = panel.copy
      const isLastPanel = idx === PANEL_STEPS.length - 1
      primaryAction = {
        label: isLastPanel ? 'drop into the shell' : 'next',
        testId: isLastPanel ? 'tour-overlay-shell-button' : 'tour-overlay-next-button',
        onClick: () => {
          advanceOnboarding(step)
            .then(() => {
              // Hard navigation: every cached query is re-issued after
              // the step changes. Each step lands on the next page the
              // tour describes; the last hands off to the shell.
              window.location.assign(isLastPanel ? '/shell' : PANEL_STEPS[idx + 1].route)
            })
            .catch(e => console.error('panel-tour next:', e))
        },
      }
    }
  } else if (isGraduate) {
    copy = GRADUATE_COPY
    progress = 'tour: done'
    primaryAction = {
      label: 'finish the tour',
      testId: 'tour-overlay-finish-button',
      onClick: () => {
        advanceOnboarding(step)
          .then(() => {
            // Graduation revoked Loom; drop the stored org so /me
            // doesn't 403, and land on the home org.
            localStorage.removeItem('weft_org')
            window.location.assign('/dashboards')
          })
          .catch(e => console.error('graduate:', e))
      },
    }
  } else {
    // Shell phase. Copy comes from the server; the user advances by
    // running the lesson's Action verbatim (we pre-inject it into the
    // input).
    copy = user.onboarding_prompt || `Lesson ${step}…`
    progress = 'tour: shell'
    // Following the sidebar highlights ends on a panel page with a shell
    // card but no shell; give the user the way in.
    if (location.pathname !== '/shell') {
      primaryAction = {
        label: 'drop into the shell',
        testId: 'tour-overlay-shell-button',
        onClick: () => navigate('/shell'),
      }
    }
  }

  const shellAction = isShell ? user.onboarding_action : undefined

  return (
    <div className="tour-overlay" role="dialog" aria-label={progress} data-testid="tour-overlay">
      <div className="tour-overlay-progress" data-testid="tour-overlay-progress">
        {progress}
      </div>
      <div className="tour-overlay-copy" data-testid="tour-overlay-copy">
        {renderPromptCopy(copy)}
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
          <span className="tour-overlay-paste-prompt" aria-hidden="true">$</span>
          <code className="tour-overlay-paste-text">{shellAction}</code>
          <span className="tour-overlay-paste-hint" aria-hidden="true">paste</span>
        </button>
      )}
      {primaryAction && (
        <div className="tour-overlay-actions">
          <button
            className="tour-overlay-button tui-btn tui-btn-primary"
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
