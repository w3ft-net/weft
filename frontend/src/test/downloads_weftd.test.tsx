// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

vi.mock('../components/SidebarNav', () => ({ default: () => null }))

import Downloads from '../pages/Downloads'

// The release ext publishes the weftd deb only as
// weftd/v<ver>/weftd_<ver>_<arch>.deb; a "latest" deb does not exist and
// 403s. <ver> must track cmd/weftd/version.go.
const goVersion = /^var Version = "(\d+\.\d+\.\d+)"/m.exec(
  readFileSync(resolve(process.cwd(), '../cmd/weftd/version.go'), 'utf8'),
)![1]

describe('Downloads page weftd links', () => {
  afterEach(cleanup)

  it.each(['amd64', 'arm64'])('%s link is the versioned deb', arch => {
    render(<Downloads />)
    const href = screen.getByTestId(`downloads-weftd-${arch}-link`).getAttribute('href')
    expect(href).toBe(`https://releases.w3ft.net/weftd/v${goVersion}/weftd_${goVersion}_${arch}.deb`)
  })

  it('install command uses the same versioned file', () => {
    render(<Downloads />)
    const block = screen.getByTestId('downloads-agent-install-block').textContent ?? ''
    expect(block).toContain(`weftd/v${goVersion}/weftd_${goVersion}_amd64.deb`)
    expect(block).toContain(`dpkg -i weftd_${goVersion}_amd64.deb`)
    expect(block).not.toContain('latest')
  })
})
