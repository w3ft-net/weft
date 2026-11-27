// cors_contract.test.ts — the cross-language guard that would have
// caught this bug, and the X-Active-Org one before it.
//
// The browser shell client (frontend/src/wasm/network.ts) is TypeScript;
// the api server's CORS allow-list is Go. Nothing connected the two, so
// a header added to the TS client without updating Go CORS shipped
// twice: the browser blocked every shell call at preflight ("cannot
// reach API server") while the CLI — which does no preflight — kept
// working, hiding it.
//
// No request-driven test catches this: fetch/net/http ignore CORS; only
// a browser enforces it. So this asserts the contract statically — every
// header authHeaders() emits must be in the Go source of truth
// (clientwire/headers.go in the core module, which api.CORSMiddleware builds
// Access-Control-Allow-Headers from). Add a header on the TS side and
// forget Go → red here.

import { execFileSync } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, it, expect, beforeAll } from 'vitest'

beforeAll(() => {
  const store = new Map<string, string>([
    ['weft_token', 'jwt-for-test'],
    ['weft_org', 'acme'],
  ])
  ;(globalThis as Record<string, unknown>).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  }
  // Vite stamps this at build time; vitest doesn't apply vite's define.
  ;(globalThis as Record<string, unknown>).__WEFT_WEB_SHA__ = 'testsha'
})

const CORE = 'github.com/w3ft-net/weft'

// The core module's clientwire/headers.go: the vendored copy when
// scripts/vendor-core.sh has run, else the module the go.mod pins.
function coreHeadersFile(): string {
  const root = resolve(__dirname, '../../..')
  const vendored = join(root, 'vendor', CORE, 'clientwire/headers.go')
  if (existsSync(vendored)) return vendored
  const dir = execFileSync('go', ['list', '-mod=mod', '-m', '-f', '{{.Dir}}', CORE], {
    cwd: root,
    encoding: 'utf8',
    env: { ...process.env, GOWORK: 'off', GOFLAGS: '' },
  }).trim()
  return join(dir, 'clientwire/headers.go')
}

// Pull the quoted entries out of `var AuthHeaders = []string{ ... }`.
function goAllowedHeaders(): string[] {
  const src = readFileSync(coreHeadersFile(), 'utf8')
  const block = src.match(/var AuthHeaders = \[\]string\{([^}]*)\}/)
  if (!block) throw new Error('could not find AuthHeaders in clientwire/headers.go')
  return [...block[1].matchAll(/"([^"]+)"/g)].map((m) => m[1].toLowerCase())
}

describe('browser ↔ server CORS header contract', () => {
  it('every header the browser shell sends is in the Go CORS allow-list', async () => {
    const { authHeaders } = await import('../wasm/network')
    const allowed = goAllowedHeaders()

    for (const name of Object.keys(authHeaders())) {
      expect(
        allowed,
        `frontend/src/wasm/network.ts sends "${name}" but the core's clientwire/headers.go ` +
          `(which api.CORSMiddleware derives Access-Control-Allow-Headers from) does not list ` +
          `it — the browser shell will block every cross-origin call at preflight and report ` +
          `"cannot reach API server" while the CLI keeps working. Add "${name}" to AuthHeaders.`,
      ).toContain(name.toLowerCase())
    }
  })

  it('pins X-Weft-Client specifically (the header this incident was about)', async () => {
    const { authHeaders } = await import('../wasm/network')
    expect(Object.keys(authHeaders())).toContain('X-Weft-Client')
    expect(goAllowedHeaders()).toContain('x-weft-client')
  })
})
