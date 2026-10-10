// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

const advanceOnboarding = vi.fn()
vi.mock('../lib/api', () => ({ advanceOnboarding: (s: string) => advanceOnboarding(s) }))
vi.mock('../lib/AuthContext', () => ({
  useAuth: () => ({
    user: {
      customer: 'loom',
      onboarding_step: 'dashboards', // last panel step: button reads "drop into the shell"
      onboarding_dismissed: false,
    },
    refreshUser: vi.fn().mockResolvedValue(undefined),
  }),
}))

import TourOverlay from '../components/TourOverlay'

describe('TourOverlay: last panel step hands off to the shell', () => {
  const loc = { reload: vi.fn(), assign: vi.fn(), pathname: '/namespaces' }

  beforeEach(() => {
    advanceOnboarding.mockResolvedValue(undefined)
    vi.stubGlobal('location', loc)
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  it('navigates to /shell after advancing, instead of reloading the current page', async () => {
    render(
      <MemoryRouter initialEntries={['/namespaces']}>
        <TourOverlay />
      </MemoryRouter>,
    )
    fireEvent.click(screen.getByTestId('tour-overlay-shell-button'))
    await waitFor(() => expect(advanceOnboarding).toHaveBeenCalledWith('dashboards'))
    await waitFor(() => expect(loc.assign).toHaveBeenCalledWith('/shell'))
    expect(loc.reload).not.toHaveBeenCalled()
  })
})
