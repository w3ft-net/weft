import { describe, it, expect } from 'vitest'
import {
  PrefixFsm, applyCommand, leaf, leaves, layoutRects, directionalNeighbor, closeLeaf, splitLeaf,
  CTRL_B, ARROW_LEFT, ARROW_RIGHT, ARROW_UP, ARROW_DOWN,
  type MuxState, type PaneNode,
} from '../wasm/paneMux'

describe('PrefixFsm', () => {
  it('forwards ordinary keystrokes untouched', () => {
    const fsm = new PrefixFsm()
    expect(fsm.feed('l')).toEqual({ kind: 'forward' })
    expect(fsm.isArmed).toBe(false)
  })

  it('arms on Ctrl-B and treats the next key as a command', () => {
    const fsm = new PrefixFsm()
    expect(fsm.feed(CTRL_B)).toEqual({ kind: 'arm' })
    expect(fsm.isArmed).toBe(true)
    expect(fsm.feed('%')).toEqual({ kind: 'command', key: '%' })
    expect(fsm.isArmed).toBe(false)
    expect(fsm.feed('a')).toEqual({ kind: 'forward' })
  })

  it('an arrow after the prefix is delivered as a command key', () => {
    const fsm = new PrefixFsm()
    fsm.feed(CTRL_B)
    expect(fsm.feed(ARROW_RIGHT)).toEqual({ kind: 'command', key: ARROW_RIGHT })
  })
})

const seq = () => {
  let n = 100
  return () => `new-${++n}`
}

// A tree: [ a | (b / c) ]  — 'a' on the left, 'b' over 'c' on the right.
function sampleTree(): PaneNode {
  return {
    kind: 'split', dir: 'row',
    a: leaf('a'),
    b: { kind: 'split', dir: 'col', a: leaf('b'), b: leaf('c') },
  }
}

describe('tree ops', () => {
  it('splitLeaf % (row) puts the new pane beside the focused one', () => {
    const root = splitLeaf(leaf('a'), 'a', 'row', 'z')
    expect(root).toEqual({ kind: 'split', dir: 'row', a: leaf('a'), b: leaf('z') })
    expect(leaves(root)).toEqual(['a', 'z'])
  })

  it('splitLeaf " (col) stacks the new pane below the focused one', () => {
    const root = splitLeaf(leaf('a'), 'a', 'col', 'z')
    expect(root.kind === 'split' && root.dir).toBe('col')
  })

  it('closeLeaf collapses the parent split, promoting the sibling', () => {
    const root = closeLeaf(sampleTree(), 'c')
    expect(root).toEqual({ kind: 'split', dir: 'row', a: leaf('a'), b: leaf('b') })
  })

  it('closeLeaf of the only pane returns null', () => {
    expect(closeLeaf(leaf('a'), 'a')).toBeNull()
  })

  it('layoutRects tiles proportionally', () => {
    const rects = layoutRects(sampleTree())
    expect(rects.get('a')).toEqual({ x: 0, y: 0, w: 0.5, h: 1 })
    expect(rects.get('b')).toEqual({ x: 0.5, y: 0, w: 0.5, h: 0.5 })
    expect(rects.get('c')).toEqual({ x: 0.5, y: 0.5, w: 0.5, h: 0.5 })
  })
})

describe('directionalNeighbor', () => {
  const root = sampleTree() // [ a | (b/c) ]
  it('right from a lands on the vertically-overlapping upper pane b', () => {
    expect(directionalNeighbor(root, 'a', 'right')).toBe('b')
  })
  it('left from b (and c) lands on a', () => {
    expect(directionalNeighbor(root, 'b', 'left')).toBe('a')
    expect(directionalNeighbor(root, 'c', 'left')).toBe('a')
  })
  it('down from b lands on c; up from c lands on b', () => {
    expect(directionalNeighbor(root, 'b', 'down')).toBe('c')
    expect(directionalNeighbor(root, 'c', 'up')).toBe('b')
  })
  it('no neighbor in a direction returns null', () => {
    expect(directionalNeighbor(root, 'a', 'left')).toBeNull()
    expect(directionalNeighbor(root, 'b', 'up')).toBeNull()
  })
})

describe('applyCommand', () => {
  const base: MuxState = { root: leaf('a'), focused: 'a', zoomed: null }

  it('% splits row and focuses the new pane', () => {
    const next = applyCommand(base, 'a', '%', seq())
    expect(next.root).toEqual({ kind: 'split', dir: 'row', a: leaf('a'), b: leaf('new-101') })
    expect(next.focused).toBe('new-101')
  })

  it('" splits col', () => {
    const next = applyCommand(base, 'a', '"', seq())
    expect(next.root.kind === 'split' && next.root.dir).toBe('col')
  })

  it('arrow keys move focus directionally', () => {
    const state: MuxState = { root: sampleTree(), focused: 'a', zoomed: null }
    expect(applyCommand(state, 'a', ARROW_RIGHT, seq()).focused).toBe('b')
    const atB: MuxState = { ...state, focused: 'b' }
    expect(applyCommand(atB, 'b', ARROW_DOWN, seq()).focused).toBe('c')
    expect(applyCommand(atB, 'b', ARROW_LEFT, seq()).focused).toBe('a')
    // No pane above b — stays put.
    expect(applyCommand(atB, 'b', ARROW_UP, seq()).focused).toBe('b')
  })

  it('x closes the focused pane and refocuses a survivor', () => {
    const state: MuxState = { root: sampleTree(), focused: 'c', zoomed: null }
    const next = applyCommand(state, 'c', 'x', seq())
    expect(leaves(next.root)).toEqual(['a', 'b'])
    expect(next.focused).toBe('a')
  })

  it('x never closes the last pane', () => {
    expect(applyCommand(base, 'a', 'x', seq())).toEqual(base)
  })

  it('closing a zoomed pane clears zoom', () => {
    const state: MuxState = { root: sampleTree(), focused: 'c', zoomed: 'c' }
    expect(applyCommand(state, 'c', 'x', seq()).zoomed).toBeNull()
  })

  it('o cycles focus through leaves and wraps', () => {
    const state: MuxState = { root: sampleTree(), focused: 'c', zoomed: null }
    expect(applyCommand(state, 'c', 'o', seq()).focused).toBe('a') // wrap
  })

  it('z toggles zoom on the issuing pane', () => {
    expect(applyCommand(base, 'a', 'z', seq()).zoomed).toBe('a')
    const zoomed: MuxState = { ...base, zoomed: 'a' }
    expect(applyCommand(zoomed, 'a', 'z', seq()).zoomed).toBeNull()
  })

  it('digit keys jump to the Nth leaf', () => {
    const state: MuxState = { root: sampleTree(), focused: 'a', zoomed: null }
    expect(applyCommand(state, 'a', '2', seq()).focused).toBe('b')
    expect(applyCommand(state, 'a', '3', seq()).focused).toBe('c')
    expect(applyCommand(state, 'a', '9', seq())).toEqual(state) // out of range
  })

  it('unknown keys are a no-op', () => {
    expect(applyCommand(base, 'a', 'q', seq())).toEqual(base)
  })
})
