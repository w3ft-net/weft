# Reproducing vimgym's internal dashboard in weft

Status: proposed (2026-06-10)

Decisions (Paul, 2026-06-10): transport = **weftd socket**; token carries
**env=prod** (4-segment paths `/log/vimgym/prod/api/...`); scope =
**reproduce-only** (Phases 1-3; Phase 4 deferred). Next step: review this doc —
no code or proposals staged yet.

Goal: retire vimgym's bespoke `/admin` dashboard in favour of an equivalent
view assembled entirely inside weft — vimgym emits its metrics through the
weft Go SDK, and weft crons + dashboard tiles reproduce every panel.

## 1. What vimgym's dashboard is today

Location: React component `frontend/src/AdminDashboard.tsx`, fed by
`GET /api/stats` (`cmd/server/main.go:2729-2754`), polled **every 1s**, superuser-gated.

It is **100% in-memory / runtime state** — no DB, no logs. Two data origins:

- **Go runtime** — `runtime.NumGoroutine()`, `runtime.MemStats{Alloc,Sys}`,
  and process `startTime`.
- **In-memory game state** — `RoomManager.Stats()` (`pkg/game/room.go:47-64`:
  `Rooms`, `ActiveMatches`, `Players`, `Bots`, `Spectators`) plus the session
  registry (`len(sessions)`, `cmd/server/main.go:150-151`).

Ten live values + four 120-point sparklines (2 min of 1s samples):

| # | Panel | Type | Source today |
|---|-------|------|--------------|
| 1 | Goroutines | gauge + sparkline | `runtime.NumGoroutine()` |
| 2 | Sessions | gauge | `len(sessions)` |
| 3 | Lobbies | gauge | `Stats().Rooms` |
| 4 | Matches | gauge + sparkline | `Stats().ActiveMatches` |
| 5 | Players | gauge + sparkline | `Stats().Players` |
| 6 | Bots | gauge | `Stats().Bots` |
| 7 | Spectators | card | `Stats().Spectators` |
| 8 | Heap MB | card + sparkline | `MemStats.Alloc` |
| 9 | System MB | card | `MemStats.Sys` |
| 10 | Uptime | card | `time.Since(startTime)` |

**The gap that matters:** weft analyses *logs over time*. A gauge like
"players right now" is instantaneous in-memory state that exists nowhere in
vimgym's output. So the core of this work is **making vimgym emit those
samples** — once they're a queryable log source, the weft side is glue.

## 2. Target architecture

```
vimgym (Go)                         weft
-----------                         ----
weftsdk.RuntimeStats ──┐
  every 30s            │  weftd     /log/vimgym/prod/api/runtime.log   ── crons ──┐
                       ├─ socket ─▶ /log/vimgym/prod/api/gamestate.log ── crons ──┤─▶ dashboard tiles
gamestate ticker  ─────┘            /log/vimgym/prod/api/events.log    ── crons ──┘
  every 30s                         (events deferred — Phase 4)
```

- vimgym's namespace is `vimgym` and the token carries `env=prod`, both
  token-derived and authoritative — the coordinator stamps them on every row
  regardless of what the app claims.
- App string `"api/<kind>"` → path `/log/vimgym/prod/api/<kind>.log`
  (ns/env/svc/kind, the 4-segment layout). RuntimeStats already uses this
  convention internally (`service + "/runtime"`).
- Transport: **weftd** on the vimgym box exposes `/var/run/weftd.sock` (records);
  events, heartbeats, and runtime stats all ride it (vimgym sets
  `WithSocket("/var/run/weftd/weftd.sock")` per `.kevcons.yaml`). No HTTP
  fallback needed in prod.
- **Resolution tradeoff:** weft crons fire at most every 1 min, and tiles plot
  the last ~50 cron runs. So the weft view is 1-min resolution over ~50 min,
  vs. the old 1s/2-min real-time view. Acceptable — weft is a triage surface,
  not a live console. Emit cadence (30s) just needs to be ≤ cron cadence.

## 3. Phase 1 — instrument vimgym (the only code change)

Add the SDK (no source `replace`; it's a published module):

```
# in ~/dev/olympics
go get github.com/w3ft-net/weft-go@v0.0.0-20260430093444-94ff76c4a30e
```

In `cmd/server/main.go`, near startup (after `roomMgr`/`sessions` exist):

```go
import weftsdk "github.com/w3ft-net/weft-go"

wc, _ := weftsdk.New() // noop transport when no socket+token — safe everywhere
defer wc.Close()

// Tiles 1, 8, 9, 10 come free from runtime telemetry.
go wc.RuntimeStats(ctx, "api", weftsdk.DefaultRuntimeStatsInterval)
go wc.Heartbeat(ctx, "api", weftsdk.DefaultHeartbeatInterval)

// Tiles 2-7: sample the same in-memory state /api/stats reads, on a ticker.
go func() {
    t := time.NewTicker(30 * time.Second)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            st := roomMgr.Stats()
            sessionsMu.Lock(); nSess := len(sessions); sessionsMu.Unlock()
            _ = wc.Send("api/gamestate", map[string]any{
                "ts":             time.Now().UTC().Format(time.RFC3339Nano),
                "sessions":       nSess,
                "lobbies":        st.Rooms,
                "active_matches": st.ActiveMatches,
                "players":        st.Players,
                "bots":           st.Bots,
                "spectators":     st.Spectators,
            })
        }
    }
}()
```

Notes:
- The gamestate snapshot is **required** — it is the only place "current
  players/matches/etc." is observable. RuntimeStats covers goroutines/heap/sys/uptime.
- `New()` returning a noop transport on dev boxes means this is safe to compile
  in and run anywhere; it only ships data where weftd or a token is present.
- Refactor `/api/stats`'s snapshot into a small helper so the HTTP handler and
  the ticker share one code path (avoids drift). Keep `/api/stats` during
  migration; delete the React dashboard only once the weft view is trusted.

Emitted field map (what the pipelines below key off):

- `runtime.log`: `goroutines`, `heap_alloc_mb`, `heap_sys_mb`, `uptime_s` (+ gc/fd extras)
- `gamestate.log`: `sessions`, `lobbies`, `active_matches`, `players`, `bots`, `spectators`

## 4. Phase 1.5 — ops prerequisites

- **Producer token** for namespace `vimgym`, `env=prod` (4-seg layout →
  `/log/vimgym/prod/api/...`).
- **weftd** running on the vimgym box, exposing the records socket
  `/var/run/weftd/weftd.sock`. weftd's configured namespace/env must match the
  token. vimgym points the SDK at that path via `WithSocket(...)`; heartbeats
  ride the same socket (no separate heartbeat socket).
- Verify the path layout with `weft.ls /log/vimgym/prod/...` after first deploy
  before building crons on assumed paths.

## 5. Phase 2 — weft crons (one single-value cron per gauge/sparkline)

Each cron tails the latest sample and emits one number; single-row mode
auto-parses stdout to the stored `Value`. Schedule `* * * * *` (every minute).
Pipelines (4-seg paths, `env=prod`):

| Cron name | Pipeline |
|-----------|----------|
| `vg-goroutines` | `cat /log/vimgym/prod/api/runtime.log \| tail -1 \| jq '.goroutines'` |
| `vg-heap-mb` | `cat /log/vimgym/prod/api/runtime.log \| tail -1 \| jq '.heap_alloc_mb'` |
| `vg-sys-mb` | `cat /log/vimgym/prod/api/runtime.log \| tail -1 \| jq '.heap_sys_mb'` |
| `vg-uptime-s` | `cat /log/vimgym/prod/api/runtime.log \| tail -1 \| jq '.uptime_s'` |
| `vg-sessions` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.sessions'` |
| `vg-lobbies` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.lobbies'` |
| `vg-matches` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.active_matches'` |
| `vg-players` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.players'` |
| `vg-bots` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.bots'` |
| `vg-spectators` | `cat /log/vimgym/prod/api/gamestate.log \| tail -1 \| jq '.spectators'` |

Staged via `weft_cron_propose({name, schedule:"* * * * *", pipeline})` each,
then committed with one `weft.proposals_apply([...])`.

> Optional compaction: a single `table`-tile cron that emits all current
> values as `name value` rows for a compact "current state" panel, instead of
> (or alongside) the per-metric counters. The per-metric crons are still needed
> for sparkline history.

## 6. Phase 3 — weft dashboard (one propose payload)

`weft_dashboard_propose({ name: "vimgym", body: <yaml> })` then apply. Tile
kinds available: `sparkline`, `counter`, `gauge`, `table`; each binds to a cron
by `watch:` name and plots that cron's recent results.

```yaml
title: vimgym
sections:
  - title: Live game state
    tiles:
      - {kind: gauge,     watch: vg-players,    title: Players,    size: lg, threshold: {warn: 200, crit: 400, max: 500}}
      - {kind: sparkline, watch: vg-players,    title: Players (1h)}
      - {kind: gauge,     watch: vg-matches,    title: Matches,    size: lg}
      - {kind: sparkline, watch: vg-matches,    title: Matches (1h)}
      - {kind: counter,   watch: vg-sessions,   title: Sessions,   format: "%d"}
      - {kind: counter,   watch: vg-lobbies,    title: Lobbies,    format: "%d"}
      - {kind: counter,   watch: vg-bots,       title: Bots,       format: "%d"}
      - {kind: counter,   watch: vg-spectators, title: Spectators, format: "%d"}
  - title: Runtime
    tiles:
      - {kind: gauge,     watch: vg-goroutines, title: Goroutines, threshold: {warn: 2000, crit: 5000, max: 8000}}
      - {kind: sparkline, watch: vg-goroutines, title: Goroutines (1h)}
      - {kind: gauge,     watch: vg-heap-mb,    title: Heap MB,    threshold: {warn: 512, crit: 1024, max: 2048}}
      - {kind: sparkline, watch: vg-heap-mb,    title: Heap MB (1h)}
      - {kind: counter,   watch: vg-sys-mb,     title: System MB,  format: "%.0f MB"}
      - {kind: counter,   watch: vg-uptime-s,   title: Uptime,     format: "%.0f s"}
```

(Thresholds are placeholders — tune against real traffic. Uptime as raw seconds
is the honest mapping; a friendlier H:M:M format would need a small formatting
verb or a Lua bin/ script — note, don't block on it.)

This reproduces all 10 panels + all 4 sparklines.

## 7. Phase 4 (DEFERRED — not in current scope) — net-new metrics

Out of scope for now (Paul chose reproduce-only). Recorded for later.

Once vimgym emits structured events (a bigger lift than the snapshot ticker —
its current logs are unstructured `log.Printf` with `[auth]`/`[ws]`/`bot:`
prefixes), weft unlocks metrics the in-memory dashboard never had. Only pursue
on explicit ask (keep Phase 1-3 to faithful reproduction):

- Convert key events to `wc.Send("api/events", {...})`: signup, login
  success/failure, match start/finish, bot creation, panics/errors.
- Derive: signups/hr, login failure rate, match completion ratio, error-rate
  cron with `| alert above <n>` for push notifications (the old dashboard had
  no alerting at all).

## 8. Sequencing & verification

1. Phase 1 code in vimgym; `go build` it (compiles even without weftd).
2. Phase 1.5 token/weftd; deploy; `weft.ls /log/vimgym/...` shows both files;
   `weft.cat .../gamestate.log | tail` shows JSON snapshots.
3. Propose + apply the 10 crons (Phase 2); wait a few minutes; confirm each has
   results.
4. Propose + apply the dashboard (Phase 3); open `dashboard vimgym` in the shell
   / the web view; compare side-by-side with `/admin` under live traffic.
5. Once trusted, remove `AdminDashboard.tsx` + `/api/stats` (separate vimgym PR).

## Resolved

- Transport: **weftd socket** (vimgym box runs weftd).
- Path layout: **4-segment, env=prod**.
- Scope: **reproduce-only** (Phases 1-3).

## Still to confirm before Phase 1.5

- Where does vimgym actually run, and is weftd already deployable there? (Needed
  to write the token + weftd config steps concretely.)
