# Style review: docs/shell/man-range.md
Verified: ✅ 38 · ❌ 0 · ⚠️ 5 · 🔒 0 · 🖥 5 · ∅ 0   (copy from the ledger; recount the verdict column)
Audience: customer-facing web+shell   Words: ~340

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [x] 1. Opens with what the reader accomplishes — NAME and SYNOPSIS state it.
- [x] 2. Task-oriented sentence-case headings, no filler — man-page uppercase sections follow house format; sub-headings L36/57/69 are all caps for the same reason.
- [?] 3. Every code block is copy-pasteable and runs as written — all examples verified, but L87 gloss hides the exclusive end (range-47).
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — no output shown; L43 timezone display of zoned literals is unclear (range-16).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [?] 10. Voice — L41-42 "there is no roll-forward" is fine; L43 "server-local" is vague for users in another zone.
- [x] 11. Cross-references — none in the doc; `man range` and `range --help` are 🖥 (range-01).
- [x] 12. Renders cleanly in both targets (no stray {{ }}, raw &lt;, broken tables/fences)
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| range-47 | 87 | `range 04-01..05-14      current-year date span (Apr 1 → May 14)` | `range 04-01..05-14      current-year span, Apr 1 00:00 up to (not including) May 14 00:00` |
| range-48 | 63-67 | (units not listed) | "Units: m (minutes), h (hours), d (days). Seconds and weeks are not supported." |
| range-16 | 43 | "Server-local timezone unless the literal carries one." | add: "Output shows times as written; a zone-qualified literal is not converted to server-local." (ledger) |
| range-10 | 31 | `range 1d..` always means the last 24 hours | holds at set time; per-query re-resolution needs a human (no doc change) |

## Needs your decision first (product vs doc)
- range-16: zone-qualified ranges display their own wall clock, not converted. Document as-is, or convert the display to server-local.
- range-16: "server-local" is the server's zone, not the user's. Say which zone applies, or let the org/user pick one.
- range-01: `range --help` prints `error: expected <start>..<end>` under `sh -c`, not this page. Route `--help` to the man page, or accept as-is (interactive check pending).

## Notes
- Reversed or equal ranges give `error: start must be before end` (e.g. `range 10m..1h`); undocumented, worth one line.
- `2026-04-12T10:00Z` (no seconds, with Z) is rejected; only the full RFC3339 form takes `Z` (matches L46).
- Persistence and per-query re-resolution (range-07/09/27) cannot be tested via `sh -c`; need an interactive session.
- Default window is the last 1d when unset; the doc never says.
- `help builtins` L96 says `range` is "show or set"; consistent with this page, but help-index L28 says only "set".

## Applied 2026-10-12
Changed: units m/h/d only (no s, w, combined); exclusive-end gloss on 04-01..05-14. Left: range-16 timezone display, range --help, reversed-range error, default 1d.
