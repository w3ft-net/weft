// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'

const fetchPipelineLines = vi.fn((_pipeline?: string) => new Promise<string[]>(() => {})) // stays loading by default
const reportModalClosed = vi.fn()
vi.mock('../wasm/network', () => ({
  fetchPipelineLines: (p: string) => fetchPipelineLines(p),
  reportModalClosed: (p: string) => reportModalClosed(p),
}))
const listRows = [
  { TraceID: 't1', StartNs: 0, EndNs: 0, DurNs: 0, Dur: '1.2s', Age: '3m', Lines: 7, Errors: 2, NoLevel: false, Services: ['acme/api', 'acme/orders'], Root: 'GET /orders/812' },
  { TraceID: 't2', StartNs: 0, EndNs: 0, DurNs: 0, Dur: '5ms', Age: '20s', Lines: 1, Errors: 0, NoLevel: true, Services: null, Root: '' },
]
vi.mock('../wasm/traceExports', () => ({
  parseTraceList: () => listRows,
  traceIDPipeline: (id: string) => `trace --id ${id}`,
  samplesPipeline: () => 'trace --spans --name x --samples',
  parseTraceRows: () => [],
  renderSpanRollup: () => [],
  renderSpanAggregate: () => [],
  renderSpanSamples: () => [],
}))

import ShellTraceModal from '../components/ShellTraceModal'

const spec = { pipeline: 'trace --id abc', title: 'abc', mode: 'gap' as const }

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
    expect(reportModalClosed).toHaveBeenCalledWith('trace --id abc')
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

// A bare `trace` lists the recent traces; a row opens that trace in the same
// modal, and Back returns to the list.
describe('ShellTraceModal trace list', () => {
  const listSpec = { pipeline: 'trace', title: 'recent traces', mode: 'list' as const }

  afterEach(() => {
    cleanup()
    vi.clearAllMocks()
  })

  it('renders a row per trace, with a - where errors are unknown', async () => {
    fetchPipelineLines.mockResolvedValueOnce(['row'])
    render(<ShellTraceModal spec={listSpec} onClose={() => {}} />)
    await act(async () => {})
    expect(screen.getByTestId('shell-trace-list-table')).toBeTruthy()
    const first = screen.getByTestId('shell-trace-list-row-t1')
    expect(first.textContent).toContain('GET /orders/812')
    expect(first.textContent).toContain('acme/api, acme/orders')
    expect(first.textContent).toContain('3m')
    expect(screen.getByTestId('shell-trace-list-row-t2').textContent).toContain('-')
  })

  it('opens a trace on click and returns on Back', async () => {
    fetchPipelineLines.mockResolvedValueOnce(['row'])
    render(<ShellTraceModal spec={listSpec} onClose={() => {}} />)
    await act(async () => {})

    fetchPipelineLines.mockResolvedValueOnce([])
    fireEvent.click(screen.getByTestId('shell-trace-list-row-t1'))
    await act(async () => {})
    expect(fetchPipelineLines).toHaveBeenLastCalledWith('trace --id t1')
    expect(screen.queryByTestId('shell-trace-list-table')).toBeNull()
    expect(screen.getByRole('heading').textContent).toContain('t1')

    fetchPipelineLines.mockResolvedValueOnce(['row'])
    fireEvent.click(screen.getByTestId('shell-trace-modal-back-button'))
    await act(async () => {})
    expect(fetchPipelineLines).toHaveBeenLastCalledWith('trace')
    expect(screen.getByTestId('shell-trace-list-table')).toBeTruthy()
  })

  it('says so when there are no traces', async () => {
    fetchPipelineLines.mockResolvedValueOnce([])
    const mod = await import('../wasm/traceExports')
    vi.spyOn(mod, 'parseTraceList').mockReturnValueOnce([])
    render(<ShellTraceModal spec={listSpec} onClose={() => {}} />)
    await act(async () => {})
    expect(screen.getByText(/no traces in this range/)).toBeTruthy()
  })
})
