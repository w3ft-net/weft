// @vitest-environment jsdom
import { StrictMode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'

const verify = vi.fn()
vi.mock('../lib/api', () => ({ acceptInvite: vi.fn() }))
vi.mock('../lib/AuthContext', () => ({
  useAuth: () => ({ verify, loginWithToken: vi.fn() }),
}))

import Verify from '../pages/Verify'

describe('Verify: magic-link tokens are single-use', () => {
  afterEach(() => {
    cleanup()
    vi.clearAllMocks()
  })

  it('calls verify once per token even when StrictMode double-invokes effects', async () => {
    verify.mockResolvedValue({})
    render(
      <StrictMode>
        <MemoryRouter initialEntries={['/verify?token=abc123']}>
          <Verify />
        </MemoryRouter>
      </StrictMode>,
    )
    await waitFor(() => expect(verify).toHaveBeenCalled())
    await new Promise(r => setTimeout(r, 50))
    expect(verify).toHaveBeenCalledTimes(1)
  })
})
