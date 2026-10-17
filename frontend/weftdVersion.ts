import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

// The released weftd version, read from the Go source of truth
// (cmd/weftd/version.go) so the Downloads page links the same versioned .deb
// the release ext publishes (weftd/v<ver>/weftd_<ver>_<arch>.deb; there is no
// "latest" deb). Throws rather than falling back: a build that cannot name the
// version must fail, not ship a dead download link.
export function weftdVersion(): string {
  const file = fileURLToPath(new URL('../cmd/weftd/version.go', import.meta.url))
  const m = /^var Version = "(\d+\.\d+\.\d+)"/m.exec(readFileSync(file, 'utf8'))
  if (!m) throw new Error(`${file}: no 'var Version = "x.y.z"' line found`)
  return m[1]
}
