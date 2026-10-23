// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { TableTile } from '../pages/Dashboards'
import type { CronWatch, DashboardWatchPayload, LatestPoint } from '../lib/api'

function show(latest: LatestPoint[]) {
  const payload = { cron: { Name: 'hosts' } as CronWatch, latest, plot: null } satisfies DashboardWatchPayload
  render(<TableTile tile={{ kind: 'table', cron: 'hosts' }} payload={payload} />)
}

// A table tile is the cron's latest run, one row per tag, in the order the
// server sent (numbers highest first, text by tag).
describe('TableTile', () => {
  afterEach(cleanup)

  it('shows a tag and a value per row, in the order given', () => {
    show([{ tag: 'web-2', value: 91 }, { tag: 'web-1', value: 40 }])
    const rows = screen.getAllByTestId(/^table-hosts-row-/)
    expect(rows.map(r => r.textContent)).toEqual(['web-291', 'web-140'])
    expect(screen.getByText('tag')).toBeDefined()
  })

  it('shows text values from a text cron', () => {
    show([{ tag: 'lease-a', text: 'i-0abc' }])
    expect(screen.getByTestId('table-hosts-row-lease-a').textContent).toBe('lease-ai-0abc')
  })

  it('drops the tag column for an untagged cron', () => {
    show([{ tag: '', value: 7 }])
    expect(screen.queryByText('tag')).toBeNull()
    expect(screen.getByTestId('table-hosts-row-').textContent).toBe('7')
  })
})
