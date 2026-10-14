// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'

vi.mock('../lib/api', async importOriginal => ({
  ...(await importOriginal<typeof import('../lib/api')>()),
  inviteMember: vi.fn(),
  removeMember: vi.fn(),
  updateTeamName: vi.fn(),
}))

import { TeamCard } from '../pages/Account'

// Shape of GET /api/team members: saaskit orgs.Membership's JSON tags.
const team = {
  org_name: 'Acme',
  seats_used: 2,
  seats_limit: 5,
  members: [
    { id: 'm1', orgId: 'o1', email: 'alice@acme.com', userId: 'u1', role: 'admin', status: 'active', seated: true,
      invitedAt: '2026-01-01T00:00:00Z', acceptedAt: '2026-01-02T00:00:00Z' },
    { id: 'm2', orgId: 'o1', email: 'carol@acme.com', role: 'member', status: 'pending', seated: true,
      invitedAt: '2026-02-01T00:00:00Z' },
  ],
}

describe('TeamCard renders the /api/team wire shape', () => {
  afterEach(cleanup)

  it('shows email, role and status for each member', () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    render(<TeamCard team={team as any} onRefresh={() => {}} currentEmail="alice@acme.com" />)
    const alice = screen.getByTestId('account-team-row-m1')
    expect(alice.textContent).toContain('alice@acme.com')
    expect(alice.textContent).toContain('admin')
    expect(alice.textContent).toContain('active')
    const carol = screen.getByTestId('account-team-row-m2')
    expect(carol.textContent).toContain('carol@acme.com')
    expect(carol.textContent).toContain('invited')
  })
})
