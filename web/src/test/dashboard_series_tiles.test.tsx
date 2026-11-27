// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Tile } from '../pages/Dashboards'
import type { CronWatch, DashboardTile, DashboardWatchPayload } from '../lib/api'
import type { ChartPlot } from '../lib/chartPlot'

const payload = { cron: { Name: 'errs' } as CronWatch } satisfies DashboardWatchPayload

const barPlot: ChartPlot = {
  kind: 'bar',
  timeX: true,
  series: [{ tag: '', points: [{ x: 0, label: '10:00', y: 3 }, { x: 3600, label: '11:00', y: 9 }] }],
}

function show(tile: DashboardTile) {
  render(<MemoryRouter><Tile tile={tile} payload={payload} /></MemoryRouter>)
}

// A bar tile draws its own plot, so two tiles on one cron can differ in
// aggregate and bucket; a plot the chart refused shows the reason instead.
describe('series tiles', () => {
  afterEach(cleanup)

  it('draws a bar tile as one row per point', () => {
    show({ kind: 'bar', cron: 'errs', aggregate: 'max', bucket: '1h', plot: barPlot })
    expect(screen.getByTestId('shell-chart-modal-bar-11:00').textContent).toContain('9')
  })

  it('shows why a bar tile has no plot', () => {
    show({ kind: 'bar', cron: 'errs', error: 'a bar chart draws one series and cron "errs" has 2 tags; use --kind line' })
    expect(screen.getByTestId('tile-error-errs').textContent).toContain('has 2 tags')
  })

  it('heads a tile with its title, falling back to the cron', () => {
    show({ kind: 'bar', cron: 'errs', title: 'Worst per hour', plot: barPlot })
    expect(screen.getByText('Worst per hour')).toBeDefined()
    cleanup()
    show({ kind: 'bar', cron: 'errs', plot: barPlot })
    expect(screen.getByText('errs')).toBeDefined()
  })
})
