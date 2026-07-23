// Panes — a browser tmux over the weft shell.
//
// Each pane is an independent shellcore instance (its own history,
// cwd, prompt) booted on the single page-wide Go runtime, all sharing
// one server-side MountFS — so `echo hi > /tmp/f` in one pane is
// `cat /tmp/f`-able in another. The DOM is the compositor: panes are
// xterm.js instances positioned from the split tree's layout rects.
//
// Panes are rendered FLAT (absolutely positioned), not as nested flex,
// so restructuring the tree (split/close) never changes a pane's React
// parent and thus never remounts — and reboots — a living shell.
//
// Multiplexing lives here in React:
//   Ctrl-B  arms the prefix; the next key is a mux command:
//     %          split left/right      "          split top/bottom
//     ← ↑ → ↓    move focus directionally
//     o          cycle focus           1..9       jump to pane N
//     x          close focused pane    z          zoom (maximize) toggle
// Anything else is forwarded to the focused pane's shell.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { listSources } from '../lib/api'
import { initShellPane, type ShellBridge, type ShellIdentity } from '../wasm/bridge'
import {
  PrefixFsm, applyCommand, leaf, leaves, layoutRects, ESC,
  type MuxState, type PaneNode, type Rect,
} from '../wasm/paneMux'
import { useAuth } from '../lib/AuthContext'
import '@xterm/xterm/css/xterm.css'
import './Panes.css'

let paneCounter = 0
function nextPaneId(): string {
  paneCounter += 1
  return `pane-${paneCounter}`
}

// Each PaneView mount gets a globally-unique Go instance id. Under
// React StrictMode a pane's effect runs setup→cleanup→setup; a unique
// id per run means the disposed first run's wasmPaneClose can't evict
// the live second run's core from the registry (which would leave the
// prompt painted but input routing nowhere).
let bootSeq = 0
function nextBootInstance(id: string): string {
  bootSeq += 1
  return `${id}#${bootSeq}`
}

const FULL: Rect = { x: 0, y: 0, w: 1, h: 1 }

export default function Panes() {
  const { user } = useAuth()
  const [ready, setReady] = useState(false)
  const [root, setRoot] = useState<PaneNode>(leaf('pane-0'))
  const [focused, setFocused] = useState<string>('')
  const [zoomed, setZoomed] = useState<string | null>(null)
  const [prefix, setPrefix] = useState(false)
  const [services, setServices] = useState<string[]>([])
  const [identity, setIdentity] = useState<ShellIdentity | undefined>(undefined)

  const bridges = useRef<Map<string, ShellBridge>>(new Map())
  const fsm = useRef(new PrefixFsm())

  // Mirror mux state into a ref so the once-bound per-pane intercept
  // closures always compute against the latest layout. Synced after
  // commit; runCommand only reads it from event handlers.
  const stateRef = useRef<MuxState>({ root, focused, zoomed })
  useEffect(() => {
    stateRef.current = { root, focused, zoomed }
  }, [root, focused, zoomed])

  // Load the service catalog once, then open two panes side by side.
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const sources = await listSources().catch(() => [])
      if (cancelled) return
      setServices(Array.from(new Set(sources.map((s) => s.service))).sort())
      setIdentity(user
        ? { user: user.email.split('@')[0], customer: user.customer }
        : undefined)
      const first = nextPaneId()
      const second = nextPaneId()
      setRoot({ kind: 'split', dir: 'row', a: leaf(first), b: leaf(second) })
      setFocused(first)
      setReady(true)
    })()
    return () => { cancelled = true }
  }, [user])

  // A mux command arrived (Ctrl-B then key) while `paneId` was focused.
  // The layout transition is the pure applyCommand; we just commit it.
  // DOM focus follows declaratively via each PaneView's `focused` prop.
  const runCommand = useCallback((paneId: string, key: string) => {
    setPrefix(false)
    const next = applyCommand(stateRef.current, paneId, key, nextPaneId)
    if (next === stateRef.current) return // no-op key
    stateRef.current = next
    setRoot(next.root)
    setZoomed(next.zoomed)
    setFocused(next.focused)
  }, [])

  const makeIntercept = useCallback(
    (id: string) => (data: string, forward: (d: string) => void) => {
      const r = fsm.current.feed(data)
      if (r.kind === 'command') {
        if (r.key === ESC) { setPrefix(false); return } // cancel prefix
        runCommand(id, r.key)
      } else if (r.kind === 'arm') {
        setPrefix(true)
      } else {
        forward(data)
      }
    },
    [runCommand],
  )

  const paneList = useMemo(() => leaves(root), [root])
  const rects = useMemo(() => layoutRects(root), [root])

  if (!ready) {
    return <div className="panes-loading">booting shell…</div>
  }

  return (
    <div className="panes-root">
      <div className="panes-bar panes-bar-top">
        <span className="panes-title">weft panes</span>
        <span className="panes-hint">
          <kbd>Ctrl-B</kbd> then <kbd>%</kbd> split &#124; <kbd>"</kbd> split &#8210;{' '}
          <kbd>←↑→↓</kbd> move &#183; <kbd>o</kbd> next &#183; <kbd>x</kbd> close &#183;{' '}
          <kbd>z</kbd> zoom &#183; <kbd>1</kbd>-<kbd>9</kbd> jump
        </span>
      </div>

      <div className="panes-grid">
        {paneList.map((id) => {
          const hidden = !!zoomed && zoomed !== id
          const rect = zoomed === id ? FULL : (rects.get(id) ?? FULL)
          return (
            <PaneView
              key={id}
              id={id}
              rect={rect}
              hidden={hidden}
              focused={focused === id}
              services={services}
              identity={identity}
              makeIntercept={makeIntercept}
              onFocus={() => setFocused(id)}
              onClose={() => runCommand(id, 'x')}
              register={(bridge) => {
                if (bridge) bridges.current.set(id, bridge)
                else bridges.current.delete(id)
              }}
            />
          )
        })}
      </div>

      <div className="panes-bar panes-bar-status">
        <span>{paneList.length} pane{paneList.length === 1 ? '' : 's'}</span>
        <span className="panes-focus">focus: {focused}</span>
        {prefix && <span className="panes-prefix">PREFIX</span>}
        {zoomed && <span className="panes-zoom">ZOOM {zoomed}</span>}
      </div>
    </div>
  )
}

interface PaneViewProps {
  id: string
  rect: Rect
  hidden: boolean
  focused: boolean
  services: string[]
  identity: ShellIdentity | undefined
  makeIntercept: (id: string) => (data: string, forward: (d: string) => void) => void
  onFocus: () => void
  onClose: () => void
  register: (bridge: ShellBridge | null) => void
}

function PaneView({
  id, rect, hidden, focused, services, identity, makeIntercept, onFocus, onClose, register,
}: PaneViewProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const bridgeRef = useRef<ShellBridge | null>(null)

  useEffect(() => {
    if (!containerRef.current) return
    let disposed = false
    let bridge: ShellBridge | null = null
    // Unique per effect run so StrictMode's throwaway first mount can't
    // close the live core (see nextBootInstance).
    const bootInstance = nextBootInstance(id)

    ;(async () => {
      if (!containerRef.current || disposed) return
      bridge = await initShellPane(
        containerRef.current,
        bootInstance,
        services,
        identity,
        makeIntercept(id),
      )
      if (disposed) {
        bridge.dispose()
        return
      }
      bridgeRef.current = bridge
      register(bridge)
      if (focused) bridge.terminal.focus()
    })()

    return () => {
      disposed = true
      bridgeRef.current = null
      register(null)
      bridge?.dispose()
    }
    // Mount once per pane id; refs keep captured closures live.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  // Declarative focus: whenever this pane becomes the focused one, give
  // its terminal DOM focus (covers arrow-nav and jump-to-N). New panes
  // are handled in the init above once their bridge is ready.
  useEffect(() => {
    if (focused) bridgeRef.current?.terminal.focus()
  }, [focused])

  return (
    <div
      className={`pane${focused ? ' is-focused' : ''}`}
      data-testid={`pane-${id}`}
      style={{
        left: `${rect.x * 100}%`,
        top: `${rect.y * 100}%`,
        width: `${rect.w * 100}%`,
        height: `${rect.h * 100}%`,
        display: hidden ? 'none' : undefined,
      }}
    >
      <div className="pane-head">
        <span className="pane-name">{id}</span>
        <button
          className="pane-close"
          data-testid={`pane-close-${id}`}
          onClick={onClose}
          title="close pane"
        >
          ✕
        </button>
      </div>
      <div
        ref={containerRef}
        className="pane-term"
        data-testid={`pane-term-${id}`}
        onMouseDown={onFocus}
      />
    </div>
  )
}
