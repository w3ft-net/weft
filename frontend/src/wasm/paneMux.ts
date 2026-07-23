// paneMux — the pure logic behind the browser-tmux pane grid.
//
// Kept free of React and xterm so it can be unit-tested directly. The
// <Panes> component owns the DOM/xterm side-effects (focusing a
// terminal, mounting/disposing a pane) and drives them from the state
// this module computes.
//
// Layout is a binary split tree, like tmux: `%` splits a pane
// left/right (a 'row' node), `"` splits it top/bottom (a 'col' node).
// Panes are rendered flat (absolutely positioned from layoutRects) so
// restructuring the tree never remounts a living terminal.

// ── Prefix FSM (Ctrl-B) ─────────────────────────────────────────

export type PrefixResult =
  | { kind: 'arm' } // Ctrl-B seen; swallow, await command
  | { kind: 'command'; key: string } // the command key after a prefix
  | { kind: 'forward' } // ordinary input for the shell

export const CTRL_B = '\x02'
export const ESC = '\x1b'
export const ARROW_UP = '\x1b[A'
export const ARROW_DOWN = '\x1b[B'
export const ARROW_RIGHT = '\x1b[C'
export const ARROW_LEFT = '\x1b[D'

export class PrefixFsm {
  private armed = false

  feed(data: string): PrefixResult {
    if (this.armed) {
      this.armed = false
      return { kind: 'command', key: data }
    }
    if (data === CTRL_B) {
      this.armed = true
      return { kind: 'arm' }
    }
    return { kind: 'forward' }
  }

  get isArmed(): boolean {
    return this.armed
  }
}

// ── Split tree ──────────────────────────────────────────────────

export type PaneNode =
  | { kind: 'leaf'; id: string }
  | { kind: 'split'; dir: 'row' | 'col'; a: PaneNode; b: PaneNode }

export type Direction = 'left' | 'right' | 'up' | 'down'

export interface Rect { x: number; y: number; w: number; h: number }

export interface MuxState {
  root: PaneNode
  focused: string
  zoomed: string | null
}

export function leaf(id: string): PaneNode {
  return { kind: 'leaf', id }
}

// leaves lists pane ids left-to-right / top-to-bottom (tree order).
export function leaves(node: PaneNode, out: string[] = []): string[] {
  if (node.kind === 'leaf') out.push(node.id)
  else {
    leaves(node.a, out)
    leaves(node.b, out)
  }
  return out
}

export function firstLeaf(node: PaneNode): string {
  return node.kind === 'leaf' ? node.id : firstLeaf(node.a)
}

// splitLeaf replaces the focused leaf with a split whose first child is
// the original pane and whose second is a fresh leaf. `%` → 'row',
// `"` → 'col'.
export function splitLeaf(
  node: PaneNode,
  focusedId: string,
  dir: 'row' | 'col',
  newId: string,
): PaneNode {
  if (node.kind === 'leaf') {
    if (node.id !== focusedId) return node
    return { kind: 'split', dir, a: node, b: leaf(newId) }
  }
  return {
    ...node,
    a: splitLeaf(node.a, focusedId, dir, newId),
    b: splitLeaf(node.b, focusedId, dir, newId),
  }
}

// closeLeaf removes a pane and collapses its parent split, promoting the
// surviving sibling. Returns null if the whole tree was that one leaf.
export function closeLeaf(node: PaneNode, id: string): PaneNode | null {
  if (node.kind === 'leaf') return node.id === id ? null : node
  const a = closeLeaf(node.a, id)
  const b = closeLeaf(node.b, id)
  if (a && b) return { ...node, a, b }
  return a ?? b
}

// layoutRects assigns every leaf a rectangle in unit space (0..1). Each
// split divides its box in half along its axis; nested splits compound,
// giving proportional tmux-style tiling.
export function layoutRects(
  node: PaneNode,
  rect: Rect = { x: 0, y: 0, w: 1, h: 1 },
  out: Map<string, Rect> = new Map(),
): Map<string, Rect> {
  if (node.kind === 'leaf') {
    out.set(node.id, rect)
    return out
  }
  if (node.dir === 'row') {
    const half = rect.w / 2
    layoutRects(node.a, { ...rect, w: half }, out)
    layoutRects(node.b, { x: rect.x + half, y: rect.y, w: half, h: rect.h }, out)
  } else {
    const half = rect.h / 2
    layoutRects(node.a, { ...rect, h: half }, out)
    layoutRects(node.b, { x: rect.x, y: rect.y + half, w: rect.w, h: half }, out)
  }
  return out
}

function overlaps(a0: number, aLen: number, b0: number, bLen: number): boolean {
  return a0 < b0 + bLen && b0 < a0 + aLen
}

// directionalNeighbor picks the pane to focus when the arrow `dir` is
// pressed: the nearest pane on that side, preferring ones whose
// perpendicular extent overlaps the current pane (so Right from a tall
// left pane lands on the pane beside it, not a diagonal one).
export function directionalNeighbor(
  root: PaneNode,
  focusedId: string,
  dir: Direction,
): string | null {
  const rects = layoutRects(root)
  const me = rects.get(focusedId)
  if (!me) return null
  const meCx = me.x + me.w / 2
  const meCy = me.y + me.h / 2

  let best: string | null = null
  let bestScore = Infinity
  for (const [id, r] of rects) {
    if (id === focusedId) continue
    const cx = r.x + r.w / 2
    const cy = r.y + r.h / 2
    let primary: number
    let overlap: boolean
    switch (dir) {
      case 'left':
        if (cx >= meCx) continue
        primary = meCx - cx
        overlap = overlaps(me.y, me.h, r.y, r.h)
        break
      case 'right':
        if (cx <= meCx) continue
        primary = cx - meCx
        overlap = overlaps(me.y, me.h, r.y, r.h)
        break
      case 'up':
        if (cy >= meCy) continue
        primary = meCy - cy
        overlap = overlaps(me.x, me.w, r.x, r.w)
        break
      case 'down':
        if (cy <= meCy) continue
        primary = cy - meCy
        overlap = overlaps(me.x, me.w, r.x, r.w)
        break
    }
    const score = primary + (overlap ? 0 : 1000)
    if (score < bestScore) {
      bestScore = score
      best = id
    }
  }
  return best
}

const ARROW_DIR: Record<string, Direction> = {
  [ARROW_LEFT]: 'left',
  [ARROW_RIGHT]: 'right',
  [ARROW_UP]: 'up',
  [ARROW_DOWN]: 'down',
}

// applyCommand computes the next MuxState after a mux command `key`
// issued while `paneId` was focused. `newId` mints a fresh id for
// splits. Unknown keys leave the state unchanged. Pure: no side effects.
export function applyCommand(
  state: MuxState,
  paneId: string,
  key: string,
  newId: () => string,
): MuxState {
  // Directional focus (Ctrl-B then arrow).
  const arrowDir = ARROW_DIR[key]
  if (arrowDir) {
    const target = directionalNeighbor(state.root, paneId, arrowDir)
    return target ? { ...state, focused: target } : state
  }

  switch (key) {
    case '%':
    case '"': {
      const id = newId()
      const dir = key === '%' ? 'row' : 'col'
      return { ...state, root: splitLeaf(state.root, paneId, dir, id), focused: id }
    }
    case 'x': {
      const next = closeLeaf(state.root, paneId)
      if (!next) return state // never close the last pane
      return {
        root: next,
        focused: state.focused === paneId ? firstLeaf(next) : state.focused,
        zoomed: state.zoomed === paneId ? null : state.zoomed,
      }
    }
    case 'o': {
      const list = leaves(state.root)
      const i = list.indexOf(paneId)
      const nextId = list[(i + 1) % list.length]
      return nextId ? { ...state, focused: nextId } : state
    }
    case 'z':
      return { ...state, zoomed: state.zoomed === paneId ? null : paneId }
    default: {
      if (/^[1-9]$/.test(key)) {
        const target = leaves(state.root)[Number(key) - 1]
        if (target) return { ...state, focused: target }
      }
      return state
    }
  }
}
