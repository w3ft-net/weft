// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'

vi.mock('../components/SidebarNav', () => ({ default: () => null }))

import { ManualTopic } from '../pages/Manual'
import { manualSections } from '../generated/docs'

Element.prototype.scrollTo = () => {}

function renderTopic(slug: string) {
  render(
    <MemoryRouter initialEntries={[`/manual/${slug}`]}>
      <Routes>
        <Route path="/manual/:slug" element={<ManualTopic />} />
      </Routes>
    </MemoryRouter>,
  )
}

describe('manual pager', () => {
  afterEach(cleanup)

  it('links the neighbours at the top and bottom of a middle page', () => {
    const i = 3
    renderTopic(manualSections[i].section)
    for (const pos of ['top', 'bottom']) {
      expect(screen.getByTestId(`manual-pager-${pos}-prev-link`).getAttribute('href')).toBe(
        `/manual/${manualSections[i - 1].section}`,
      )
      expect(screen.getByTestId(`manual-pager-${pos}-next-link`).getAttribute('href')).toBe(
        `/manual/${manualSections[i + 1].section}`,
      )
    }
  })

  it('has no previous link on the first page', () => {
    renderTopic(manualSections[0].section)
    expect(screen.queryByTestId('manual-pager-top-prev-link')).toBeNull()
    expect(screen.queryByTestId('manual-pager-bottom-prev-link')).toBeNull()
    expect(screen.getByTestId('manual-pager-top-next-link')).toBeTruthy()
  })

  it('has no next link on the last page', () => {
    const last = manualSections[manualSections.length - 1]
    renderTopic(last.section)
    expect(screen.queryByTestId('manual-pager-top-next-link')).toBeNull()
    expect(screen.queryByTestId('manual-pager-bottom-next-link')).toBeNull()
    expect(screen.getByTestId('manual-pager-bottom-prev-link')).toBeTruthy()
  })
})
