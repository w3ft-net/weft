// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

const advanceOnboarding = vi.fn()
const authStep = vi.hoisted(() => ({ current: 'dashboards' }))
vi.mock('../lib/api', () => ({ advanceOnboarding: (s: string) => advanceOnboarding(s) }))
vi.mock('../lib/AuthContext', () => ({
  useAuth: () => ({
    user: {
      customer: 'loom',
      onboarding_step: authStep.current, // 'dashboards' is the last panel step: "drop into the shell"
      onboarding_dismissed: false,
    },
    refreshUser: vi.fn().mockResolvedValue(undefined),
  }),
}))

import TourOverlay from '../components/TourOverlay'

describe('TourOverlay: last panel step hands off to the shell', () => {
  const loc = { reload: vi.fn(), assign: vi.fn(), pathname: '/dashboards' }

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
      <MemoryRouter initialEntries={['/dashboards']}>
        <TourOverlay />
      </MemoryRouter>,
    )
    fireEvent.click(screen.getByTestId('tour-overlay-shell-button'))
    await waitFor(() => expect(advanceOnboarding).toHaveBeenCalledWith('dashboards'))
    await waitFor(() => expect(loc.assign).toHaveBeenCalledWith('/shell'))
    expect(loc.reload).not.toHaveBeenCalled()
  })
})

describe('TourOverlay: panel steps and the final step', () => {
  const loc = { reload: vi.fn(), assign: vi.fn(), pathname: '/' }

  beforeEach(() => {
    advanceOnboarding.mockResolvedValue(undefined)
    vi.stubGlobal('location', loc)
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
    vi.clearAllMocks()
  })

  it('off its page, a panel step offers to go there instead of describing it', () => {
    authStep.current = 'namespaces'
    render(<MemoryRouter initialEntries={['/downloads']}><TourOverlay /></MemoryRouter>)
    expect(screen.getByTestId('tour-overlay-copy').textContent).toContain('to get started')
    expect(screen.getByTestId('tour-overlay-start-button')).toBeTruthy()
    expect(document.body.dataset.tourHighlight).toBeUndefined()
  })

  it('on its page, next advances and lands on the following page', async () => {
    authStep.current = 'namespaces'
    render(<MemoryRouter initialEntries={['/namespaces']}><TourOverlay /></MemoryRouter>)
    expect(screen.getByTestId('tour-overlay-copy').textContent).toContain('Namespaces are containers')
    fireEvent.click(screen.getByTestId('tour-overlay-next-button'))
    await waitFor(() => expect(advanceOnboarding).toHaveBeenCalledWith('namespaces'))
    await waitFor(() => expect(loc.assign).toHaveBeenCalledWith('/dashboards'))
  })

  it('a panel page that redirects into its subtree still counts as on the page', () => {
    authStep.current = 'dashboards'
    render(<MemoryRouter initialEntries={['/dashboards/loom-overview']}><TourOverlay /></MemoryRouter>)
    expect(screen.getByTestId('tour-overlay-shell-button')).toBeTruthy()
    expect(screen.queryByTestId('tour-overlay-start-button')).toBeNull()
  })

  it('the graduate step waits for a click before leaving the tour', async () => {
    authStep.current = 'graduate'
    const removeItem = vi.fn()
    vi.stubGlobal('localStorage', { removeItem })
    render(<MemoryRouter initialEntries={['/shell']}><TourOverlay /></MemoryRouter>)
    expect(advanceOnboarding).not.toHaveBeenCalled()
    fireEvent.click(screen.getByTestId('tour-overlay-finish-button'))
    await waitFor(() => expect(advanceOnboarding).toHaveBeenCalledWith('graduate'))
    await waitFor(() => expect(loc.assign).toHaveBeenCalledWith('/dashboards'))
  })
})
