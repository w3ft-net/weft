# Versioning protocol

How we keep `weft` (CLI), `weftd` (agent), the frontend WASM, and the
API server in sync without silently breaking a customer when the wire
shape changes.

## TL;DR

- Each binary identifies itself on every server-bound call:
  - `weft` → HTTP `X-Weft-Client: weft/<ver>[+<sha>]`
  - `weftd` → gRPC `weft-client` metadata
  - browser → HTTP `X-Weft-Client: weft-web/<git-sha>` (no version gate;
    ships in lockstep with the API)
- API server enforces `MIN_WEFT_CLI_VERSION` and returns HTTP 426 to old
  CLIs. The CLI's HTTP code renders that as `weft update`.
- Coordinator records every agent's reported version on
  `agent_tokens.last_seen_version` and enforces `WEFTD_HARD_FLOOR` on
  IngestStream open. Between the soft minimum and the hard floor,
  weftd connections work but show up in operator tooling.
- When you change a wire shape, you bump the relevant binary's
  `Version` const. CI fails any PR that touches wire paths without
  bumping. The release ext (`kevcons ext release …`) publishes signed
  binaries to `releases.w3ft.net/<binary>/v<ver>/` plus a
  `latest/manifest.json` for auto-update.

## Where versions live

- `cmd/weft/version.go` — `Version` const, source of truth for the CLI.
- `cmd/weftd/version.go` — same, for the agent.
- `frontend/src/main.tsx` (or Vite config) — auto-stamped at build time;
  the WASM ships in lockstep with the API so there's no semver gate.

`ClientID()` in each `main` package formats these as
`<binary>/<version>[+<sha>]`. The release ext sets `GitSHA` via
`-ldflags`; dev builds leave it empty.

## When to bump

Any change that affects what bytes are sent or received over the wire:

- `proto/weftpb/*.proto`
- `api/shell.go`, `api/middleware.go`, anything in `api/` that touches
  request/response shapes
- `coordinator/service/server.go` IngestStream / Execute handlers
- `shell/http_remote.go` (CLI HTTP transport)
- `frontend/src/wasm/network.ts` (browser HTTP transport)
- `cmd/weftd/main.go` ingest plumbing
- `internal/timerange/*` (parsed server-side; behavior changes are wire
  changes)

Server-internal refactors that don't change the wire contract can opt
out by including the literal token `no-wire-change` in a commit message.

## CI tripwire

`scripts/check-wire-bump.sh` (run by the `Wire-bump tripwire` job on
every PR) compares the PR's changed files against the wire-affecting
path set above. If any are present and neither version.go was bumped,
the PR fails with a clear error.

## Tolerance windows

- **weft CLI**: no tolerance. Server returns 426; user runs
  `weft update`. The user is interactive and can recover in seconds.
- **weftd**: bounded tolerance. The server accepts old-shape parsing
  for one minor release. Each old-shape branch carries a
  `// REMOVE-WHEN: ` comment with the removal condition, e.g.
  `// REMOVE-WHEN: store.AgentsAtOrBelow("0.15.99", 14d) is empty`.
  The operator runs `/api/admin/agents` periodically to see which
  customers are still on old agents, nudges them, and deletes the
  branch when the table goes quiet.
- **Frontend**: no tolerance — same artifact as the API server.

When tolerance runs out and you want to enforce, raise
`WEFTD_HARD_FLOOR` to the new minimum. Below it, IngestStream returns
`codes.FailedPrecondition` and the customer's logs stop flowing. Use
sparingly.

## Operator surfaces

- `GET /api/admin/agents` returns every active token with its last seen
  version and timestamp. The list to scan before pulling a removal
  trigger.
- `MIN_WEFT_CLI_VERSION` env var (read per-request via
  `api/client_version.go`) — raise to pressure CLI users to update.
- `MIN_WEFTD_VERSION` and `WEFTD_HARD_FLOOR` env vars (in
  `config/runtime.go`'s Runtime overlay). Min is advisory; floor is
  enforced.

## Release flow

```
# 1. Bump version.go.
$EDITOR cmd/weft/version.go    # or cmd/weftd/version.go

# 2. Commit + push, let CI pass.
git add cmd/weft/version.go && git commit -m '…' && git push

# 3. Run the release ext (builds, tests, uploads, tags, pushes tag).
kevcons ext release weft        # or: kevcons ext release weftd
```

The ext refuses to publish if `Version` in the source file disagrees
with what it's about to embed via `-ldflags`. It also refuses to re-tag
an existing release.

After release:

- `releases.w3ft.net/weft/v<ver>/weft-<platform>` — versioned binary.
- `releases.w3ft.net/weft/latest/weft-<platform>` — moving "latest".
- `releases.w3ft.net/weft/latest/manifest.json` — the JSON consumed by
  `weft update` and weftd's selfupdate checker:

```json
{
  "binary": "weft",
  "version": "0.3.1",
  "sha": "abc123",
  "platforms": [
    {"suffix":"darwin-arm64","goos":"darwin","goarch":"arm64",
     "sha256":"…","url":"https://releases.w3ft.net/weft/v0.3.1/weft-darwin-arm64"}
  ]
}
```
