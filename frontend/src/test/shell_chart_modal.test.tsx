// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { StrictMode } from 'react'
import { MemoryRouter } from 'react-router-dom'

// recharts needs real layout; what matters here is which series reach it.
vi.mock('../components/Sparkline', async importOriginal => ({
  ...(await importOriginal<typeof import('../components/Sparkline')>()),
  Sparkline: ({ series }: { series: Array<{ points: unknown[] }> }) => (
    <div data-testid="spark-stub">{series.map(s => s.points.length).join(',')}</div>
  ),
}))
const reportModalClosed = vi.fn()
vi.mock('../wasm/network', () => ({ reportModalClosed: (p: string) => reportModalClosed(p) }))

import ShellChartModal from '../components/ShellChartModal'
import { plotToSpark, type ChartPlot } from '../lib/chartPlot'

const T0 = 1_791_633_600 // unix seconds
const line = (ys: number[]): ChartPlot => ({
  kind: 'line',
  timeX: true,
  series: [{ tag: '', points: ys.map((y, i) => ({ x: T0 + i * 300, label: `b${i}`, y })) }],
})

function show(plot: ChartPlot) {
  render(
    <MemoryRouter>
      <ShellChartModal spec={{ plot, kind: plot.kind, title: 'errors' }} onClose={() => {}} />
    </MemoryRouter>,
  )
}

describe('ShellChartModal pipe form', () => {
  afterEach(cleanup)

  it('plots a bucketed series instead of dumping the rows', () => {
    show(line([2, 5, 3]))
    expect(screen.getByTestId('spark-stub').textContent).toBe('3')
    expect(screen.queryByTestId('shell-chart-modal-pipe-lines')).toBeNull()
  })

  it('says so when everything fell into one bucket, rather than drawing nothing', () => {
    show(line([4]))
    expect(screen.queryByTestId('spark-stub')).toBeNull()
    const note = screen.getByTestId('shell-chart-modal-single-value')
    expect(note.textContent).toContain('4')
    expect(note.textContent).toMatch(/one data point/i)
  })

  it('draws a bar per category', () => {
    show({
      kind: 'bar',
      timeX: false,
      series: [{ tag: '', points: [{ x: 0, label: 'get', y: 5 }, { x: 1, label: 'put', y: 2 }] }],
    })
    expect(screen.getByTestId('shell-chart-modal-bar-get').textContent).toContain('5')
    expect(screen.getByTestId('shell-chart-modal-bar-put').textContent).toContain('2')
  })
})

describe('plotToSpark', () => {
  it('converts a time axis from unix seconds to milliseconds and keeps tags', () => {
    const [s] = plotToSpark({
      kind: 'line',
      timeX: true,
      series: [{ tag: 'web', points: [{ x: T0, label: 'a', y: 1 }] }],
    })
    expect(s.tag).toBe('web')
    expect(s.points).toEqual([{ ts: T0 * 1000, value: 1 }])
  })

  it('leaves an ordinal axis alone', () => {
    const [s] = plotToSpark({ kind: 'line', timeX: false, series: [{ tag: '', points: [{ x: 3, label: '3', y: 9 }] }] })
    expect(s.points[0].ts).toBe(3)
  })
})

// Focus sits in the terminal behind the backdrop, and xterm swallows keys
// before they bubble to window — so Esc must be caught in the capture phase.
describe('ShellChartModal Esc', () => {
  afterEach(cleanup)

  it('closes on Esc even when focus is in the terminal behind the backdrop', () => {
    const onClose = vi.fn()
    render(
      <MemoryRouter>
        <ShellChartModal spec={{ plot: line([1, 2]), kind: 'line' }} onClose={onClose} />
      </MemoryRouter>,
    )
    const termInput = document.createElement('textarea') // stands in for xterm's helper textarea
    termInput.addEventListener('keydown', e => e.stopPropagation()) // xterm consumes the key
    document.body.appendChild(termInput)
    termInput.focus()
    fireEvent.keyDown(termInput, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })
})

// The tour completes a chart lesson when its modal closes. The modal reports
// its own disappearance against the typed line, once it actually showed
// something.
describe('ShellChartModal tour report', () => {
  afterEach(() => {
    cleanup()
    vi.clearAllMocks()
  })

  it('reports a dismissed pipe chart against the line that opened it', async () => {
    const view = render(
      <MemoryRouter>
        <ShellChartModal spec={{ plot: line([1, 2]), pipeline: 'cat x | chart' }} onClose={() => {}} />
      </MemoryRouter>,
    )
    expect(reportModalClosed).not.toHaveBeenCalled()
    view.unmount()
    await new Promise(r => setTimeout(r, 20))
    expect(reportModalClosed).toHaveBeenCalledWith('cat x | chart')
    expect(reportModalClosed).toHaveBeenCalledTimes(1)
  })

  // Dev builds mount, unmount and remount every component (StrictMode). That
  // simulated unmount is not the user closing the modal.
  it('does not report the dev-mode simulated unmount of a pipe chart', async () => {
    const view = render(
      <StrictMode>
        <MemoryRouter>
          <ShellChartModal spec={{ plot: line([1, 2]), pipeline: 'cat x | chart' }} onClose={() => {}} />
        </MemoryRouter>
      </StrictMode>,
    )
    await new Promise(r => setTimeout(r, 20))
    expect(reportModalClosed).not.toHaveBeenCalled()
    view.unmount()
    await new Promise(r => setTimeout(r, 20))
    expect(reportModalClosed).toHaveBeenCalledTimes(1)
    expect(reportModalClosed).toHaveBeenCalledWith('cat x | chart')
  })

  // The shell reduces a cron's stored series to a plot before opening the
  // modal, so a cron chart draws it like a pipe chart and links to its cron.
  it('draws a cron chart from its plot, links the cron and reports the close', async () => {
    const tagged: ChartPlot = {
      kind: 'line',
      timeX: true,
      series: [
        { tag: 'api-1', points: [{ x: T0, label: 'a', y: 1 }, { x: T0 + 300, label: 'b', y: 2 }] },
        { tag: 'api-2', points: [{ x: T0, label: 'a', y: 3 }, { x: T0 + 300, label: 'b', y: 4 }] },
      ],
    }
    const view = render(
      <MemoryRouter>
        <ShellChartModal spec={{ cron: 'hosts', plot: tagged, pipeline: 'chart hosts' }} onClose={() => {}} />
      </MemoryRouter>,
    )
    expect(screen.getByTestId('spark-stub').textContent).toBe('2,2')
    expect(screen.getByTestId('shell-chart-modal-watch-link').getAttribute('href')).toBe('/cron/hosts')
    view.unmount()
    await new Promise(r => setTimeout(r, 20))
    expect(reportModalClosed).toHaveBeenCalledWith('chart hosts')
  })
})
