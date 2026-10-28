// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { TableTile } from '../pages/Dashboards'
import type { LatestPoint } from '../lib/api'

function show(points: LatestPoint[], error?: string) {
  render(<TableTile tile={{ kind: 'table', cron: 'hosts', points, error }} />)
}

// A table tile is one row per tag, in the order the server sent (numbers
// highest first, text by tag): the cron's latest run, or its aggregate over
// the tile's range.
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

  it('marks the rows that breach the cron\'s alert', () => {
    show([{ tag: 'web-2', value: 91, critical: true }, { tag: 'web-1', value: 40 }])
    expect(screen.getByTestId('table-hosts-row-web-2').className).toContain('dashboards-tone-crit')
    expect(screen.getByTestId('table-hosts-row-web-1').className).not.toContain('dashboards-tone-crit')
  })

  it('shows why a table could not aggregate', () => {
    show([], '-a p95 needs every run, and runs are kept 7 days')
    expect(screen.getByTestId('tile-error-hosts').textContent).toContain('needs every run')
  })
})
