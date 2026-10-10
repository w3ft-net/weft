// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router-dom'

vi.mock('../lib/api', () => ({ advanceOnboarding: vi.fn().mockResolvedValue(undefined) }))
vi.mock('../lib/AuthContext', () => ({
  useAuth: () => ({
    user: {
      customer: 'loom',
      onboarding_step: 'shell-ls', // first shell lesson, reached by clicking through the sidebar
      onboarding_prompt: 'Hit enter to run ls.',
      onboarding_action: 'ls /log',
      onboarding_dismissed: false,
    },
    refreshUser: vi.fn().mockResolvedValue(undefined),
  }),
}))

import TourOverlay from '../components/TourOverlay'

function Path() {
  return <div data-testid="current-path">{useLocation().pathname}</div>
}

function renderAt(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <TourOverlay />
      <Path />
    </MemoryRouter>,
  )
}

// Following the sidebar highlights ends on /dashboards with a shell-lesson
// card that says "hit enter" — but no shell on screen. The card must offer
// the way in.
describe('TourOverlay: shell lessons outside /shell', () => {
  afterEach(cleanup)

  it('offers a button that opens the shell', () => {
    renderAt('/dashboards')
    fireEvent.click(screen.getByTestId('tour-overlay-shell-button'))
    expect(screen.getByTestId('current-path').textContent).toBe('/shell')
  })

  it('does not show the button once the user is in the shell', () => {
    renderAt('/shell')
    expect(screen.queryByTestId('tour-overlay-shell-button')).toBeNull()
  })
})
