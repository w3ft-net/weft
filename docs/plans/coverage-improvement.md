# Plan: Coverage discovery + improvement

We have a working coverage gate (50% → 53% as of 2026-05-13, core at 56.0%).
This plan covers the next round of work: making coverage *gaps* more visible
so they get fixed deliberately, and lifting the floor in places we already
know are thin.

## Current state

`scripts/coverage-report.sh` runs in the `integration-coverage` job. It
merges unit + per-container integration counters with `go tool covdata`,
applies an exclusion list (`proto/weftpb`, `frontend/node_modules`, `cmd/*`,
`*_gen.go`), and emits two numbers:

- **raw** — every package
- **core** — excluding the above (this is what the gate uses)

Latest: **raw 49.7%, core 56.0%**, gate at 53%. Summary lands in
`$GITHUB_STEP_SUMMARY` plus the `coverage-profiles` artifact. The artifact
contains per-service `.out` files plus `merged.out` and `core.out` for
local browsing via `go tool cover -html=…`.

## Why more work

What today's gate doesn't catch:

1. **Per-PR regressions inside the gate's slack window.** A PR can drop core
   coverage by 2 points (56.0% → 54.0%) and still pass. We catch a 4-point
   slide; we don't catch one.
2. **Branch coverage.** `go test -cover` is statement-level. An `if err !=
   nil` with both arms statement-covered but only the happy path actually
   exercised reads as 100% — but the err arm has never run.
3. **Unevenly-distributed coverage.** Core averages 56% but the bottom
   quartile is under 30%. A flat gate doesn't pressure those.
4. **Tests that exist but don't run.** Plenty of "unit" tests skip silently
   when their environment isn't right (postgres, fixtures, etc.).
   `WEFT_ALLOW_SCHEMA_BOOTSTRAP=1` unmasked a chunk; there's almost
   certainly more.
5. **Cross-package DB pollution forced `-p 1`.** Tests using
   `TruncateTestTables` race when run in parallel. Serialization works but
   masks a real test-isolation problem.

## What to build

### 1. Per-PR coverage delta (highest leverage, ~1d)

The single biggest win. The gate enforces an absolute floor; what catches
*every* PR that loses coverage is showing the diff.

- Cache the baseline `core.out` from `main` as a GitHub Actions cache
  keyed by the commit sha (the integration-coverage job already
  uploads it, so reuse the artifact).
- On a PR, fetch the parent commit's `core.out` and diff function-level
  coverage. Comment on the PR with a table of files whose coverage
  *dropped*, plus the bottom-line delta.
- No threshold on the delta; just visibility. A PR that drops coverage
  but has a good reason can be merged after acknowledgement.

Pattern: github-action-coverage-delta-comment is a typical CI bot. We
can write our own in `scripts/coverage_diff.py` since the textfmt format
is trivially parseable and we already have all the data on disk.

### 2. Branch coverage (~2d)

`go test -cover` records statement coverage. The Go team's
[atomic counters mode](https://pkg.go.dev/cmd/go#hdr-Coverage_for_tests)
records execution counts but doesn't enumerate branch arms. To get
real branch coverage you need an instrumenting tool:

- `golang.org/x/tools/cmd/gocoverdir` produces counter mode profiles —
  we already use this via `-cover -coverpkg`.
- For *branch* coverage we'd need a separate pass with
  [gocov](https://github.com/axw/gocov) or migrate to
  [gobco](https://github.com/rillig/gobco). Both add an extra tool to
  the toolchain.

Pragmatic alternative: keep statement coverage as the gate, but pull a
branch coverage report once a release. Track the delta across releases
in a small CHANGELOG-style table in this directory. If branch coverage
drifts downward while statement coverage stays flat, that's a signal
to add error-arm tests.

### 3. Per-package floors (~0.5d setup, then ongoing)

A flat 53% gate doesn't move thin packages. Add per-package minimums
inside `scripts/coverage-report.sh`:

```
package           floor   current
store              55%     56.2%
api                40%     29.1%   ← below
shell              50%     58.9%
coordinator        60%     64.7%
planner            70%     77.0%
chart              50%     52.2%
mounts             65%     69.8%
```

Initial floors set 1-3 points below current. When a package's floor is
set to its current value, it can never regress; over time we tighten.
This catches the "coordinator covered the same code path twice but a
specific arm rotted" case that the global gate misses.

Failing the build per-package is too noisy at first. Start by surfacing
violations in the step summary, then escalate to failing the build
after we've cleaned up the initial violations.

### 4. Test-isolation cleanup — kill the `-p 1` (~3d)

`-p 1` in `.github/workflows/ci.yml` is a workaround for tests that
share the dev `weft` database and use `TruncateTestTables` in cleanup.
Root cause: blanket-truncate scripts ignore the test's own scope.

Action: replace every `db.TruncateTestTables()` test cleanup with a
scoped delete (the pattern in `coordinator/testQueryStoreReal` →
`deleteCustomerData`). Affected files:

- `shellfs/shellfs_test.go` (the `testStore` helper)
- `shellfs/perms_test.go`
- `shellfs/tmp_test.go`
- `store/team_perms_test.go`
- `api/api_test.go` (the `testServer` helper)

Each test owns a unique customer (typically `fmt.Sprintf("foo-%d",
os.Getpid())`). Cleanup deletes by customer scope, not by truncate.
After this lands, drop `-p 1` from the unit job and reclaim CI time.

`store.ExecForTest` (added 2026-05-13) is already the test-only escape
hatch.

### 5. Surgical fills for the visible 0%-packages (~2d total)

From the most recent coverage table, packages that are still at 0% or
single-digit % despite being in core:

- `coordinator/discovery` — AWS ASG bootstrap. Hard to unit-test because
  it dials EC2. Extract the pure-logic parts (e.g. tag filtering,
  instance-to-shard mapping) into testable helpers.
- `internal/email` — SES wrapper. Mostly untestable; mark for the
  exclusion list rather than chase coverage we'll never get.
- `coordinator/poller` — 8.1% currently. Small package; should be
  easy if we make the polling loop testable via dependency injection.

### 6. API package surgical work (~3d)

`api` is the second-biggest gap (91 uncovered functions, 27%). It's also
the external contract. Adding HTTP handler tests would have the highest
ROI of any single area:

- `api/handlers.go` — every endpoint that's currently uncovered. Use
  `httptest.NewServer` + the existing `testServer` helper. Wire one
  request per handler asserting status code + body shape.
- `api/dashboards.go`, `api/sources.go`, `api/usage_reporter.go` —
  similar.

Pair with #4 (kill blanket truncate) so the api tests can fan out
without poisoning the world.

### 7. Eliminate "skipping silently" — make skipped tests visible (~1d)

Tests that `t.Skip` on missing infrastructure produce no signal. CI
should fail when too many tests skip:

- gotestsum already counts skips: in the most recent CI run, 115 tests
  skipped. A trend line on this number (per CI run, per package) would
  surface drift.
- Optional gate: fail if any single package skips more than N tests
  (default 10), prompting the maintainer to either fix the env or
  remove the dead test.

This is also a way to find "shadow integration tests" — tests living
in the unit suite that quietly need the world set up. Once visible,
they get moved to the integration suite or have their setup fixed.

## Components to build

1. **`scripts/coverage_diff.py`** — parse two `core.out` files, emit
   the per-package + per-file delta as Markdown. ~150 LOC.
2. **`.github/workflows/coverage-pr.yml`** — separate workflow that runs
   on `pull_request`, downloads `main`'s coverage artifact, runs
   `coverage_diff.py`, and posts as a PR comment via `gh pr comment`.
3. **`scripts/coverage_floors.json`** — declarative per-package floors.
   `coverage-report.sh` reads it and reports violations; expand to
   gating after one release cycle of warnings.
4. **`scripts/cleanup-truncate-tests.md`** — checklist of files to
   touch when migrating off `TruncateTestTables`. Not a script — a
   tracker for the multi-PR cleanup. Closes with removing `-p 1`.
5. **Test additions** for `api/*`, `coordinator/poller`,
   `coordinator/discovery` — see §5/6.
6. **`internal/email`** — add to `EXCLUDE_PREFIXES` in
   `scripts/coverage-report.sh` (alongside `proto/weftpb`). Document
   why in the script comment.

## Estimated effort

| Item | Effort |
|---|---:|
| 1. PR coverage diff comment | 1d |
| 3. Per-package floors (warn-only first) | 0.5d |
| 7. Skip visibility | 1d |
| 4. Kill `-p 1` via cleanup | 3d |
| 6. API handler tests | 3d |
| 5. Surgical fills for visible 0% pkgs | 2d |
| 2. Branch coverage (research + report) | 2d |
| **Total** | **~12.5d** |

Sequencing: #1 first (changes the per-PR experience). Then #3 and #7
together (more visibility). Then #4 (technical debt). Then #5, #6, #2.

## Non-goals

- **End-to-end coverage targets ("90% line coverage"):** statement
  coverage past ~70% has rapidly diminishing returns; we're not chasing
  it. The point of the gate is catching *regression*, not a vanity
  number.
- **Fuzz coverage / property tests:** valuable, but out of scope —
  separate plan if/when it's worth one.
- **Mutation testing:** even more so. `go-mutesting` exists but
  integrating it adds a build dependency and a long-running CI step.
  Revisit when statement+branch coverage stops moving the needle.
- **Coverage of test code itself:** intentionally not measured.

## Cross-references

- Companion to the existing coverage gate, introduced 2026-05-13
  (`.github/workflows/ci.yml`, `scripts/coverage-report.sh`).
- `store.ExecForTest` (2026-05-13) is the test-only raw-SQL escape
  hatch used by the scoped-cleanup pattern this plan generalizes.
