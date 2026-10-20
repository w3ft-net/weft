// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'

const fetchPipelineLines = vi.fn(() => new Promise<string[]>(() => {})) // stays loading by default
const reportModalClosed = vi.fn()
vi.mock('../wasm/network', () => ({
  fetchPipelineLines: () => fetchPipelineLines(),
  reportModalClosed: (p: string) => reportModalClosed(p),
}))
vi.mock('../wasm/traceExports', () => ({
  parseTraceRows: () => [],
  renderSpanRollup: () => [],
  renderSpanAggregate: () => [],
  renderSpanSamples: () => [],
}))

import ShellTraceModal from '../components/ShellTraceModal'

const spec = { pipeline: 'trace abc', title: 'trace abc', mode: 'gap' as const }

describe('ShellTraceModal dismissal', () => {
  afterEach(() => {
    cleanup()
    vi.clearAllMocks()
  })

  it('closes on the [x] button', () => {
    const onClose = vi.fn()
    render(<ShellTraceModal spec={spec} onClose={onClose} />)
    fireEvent.click(screen.getByTestId('shell-trace-modal-close-button'))
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('closes on Esc, even when focus is in the terminal behind the backdrop', () => {
    const onClose = vi.fn()
    render(<ShellTraceModal spec={spec} onClose={onClose} />)
    const termInput = document.createElement('textarea') // stands in for xterm's helper textarea
    document.body.appendChild(termInput)
    termInput.focus()
    fireEvent.keyDown(termInput, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('does not close on other keys', () => {
    const onClose = vi.fn()
    render(<ShellTraceModal spec={spec} onClose={onClose} />)
    fireEvent.keyDown(document.body, { key: 'a' })
    expect(onClose).not.toHaveBeenCalled()
  })
})

// The tour waits for this modal to be dismissed before moving to the next
// lesson. The modal reports its own disappearance — whatever closed it (Esc,
// [x], or another modal replacing it) — but only once the trace actually
// loaded, so a failed or still-loading trace never completes the lesson.
describe('ShellTraceModal tour report', () => {
  afterEach(() => {
    cleanup()
    vi.clearAllMocks()
  })

  it('reports once when a loaded trace is dismissed', async () => {
    fetchPipelineLines.mockResolvedValueOnce(['line'])
    const view = render(<ShellTraceModal spec={spec} onClose={() => {}} />)
    await act(async () => {})
    expect(reportModalClosed).not.toHaveBeenCalled() // still open
    view.unmount()
    await new Promise(r => setTimeout(r, 20))
    expect(reportModalClosed).toHaveBeenCalledTimes(1)
    expect(reportModalClosed).toHaveBeenCalledWith('trace abc')
  })

  it('does not report a trace that was dismissed before it loaded', () => {
    const view = render(<ShellTraceModal spec={spec} onClose={() => {}} />)
    view.unmount()
    expect(reportModalClosed).not.toHaveBeenCalled()
  })

  it('does not report a trace that failed to load', async () => {
    fetchPipelineLines.mockRejectedValueOnce(new Error('boom'))
    const view = render(<ShellTraceModal spec={spec} onClose={() => {}} />)
    await act(async () => {})
    view.unmount()
    expect(reportModalClosed).not.toHaveBeenCalled()
  })
})
