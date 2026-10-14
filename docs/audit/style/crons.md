# Style review: docs/manual/crons.md
Verified: ✅ 12 · ❌ 1 · ⚠️ 11 · 🔒 5 · 🖥 7 · ∅ 0   (36 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~340

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes — l.10-13 "Set up a cron" is good, but the doc has no title-level intro; fine.
- [x] 2. Task-oriented sentence-case headings, no filler — "Set up a cron", "Accessing past results" (l.32) is slightly noun-y; ok.
- [ ] 3. Every code block copy-pasteable — l.21 uses bare `myapp.log` (crons-06); l.41-50 puts a Lua comment in a shell fence.
- [ ] 4. Realistic names — `myapp.log` is not a resolvable source; use `/log/<ns>/<env>/<svc>/main.log` (l.21).
- [x] 5. Commands/flags in `code`, UI in bold — consistent.
- [?] 6. Sample output matches verified output — l.21 example would always fire (returns 500); see decision below.
- [x] 7. No internal vocabulary — none found.
- [x] 8. Terminology: cron — consistent.
- [x] 9. No stale roadmap language; no Windows — none.
- [ ] 10. Voice — l.36-37 "return booleans" is imprecise; otherwise fine.
- [?] 11. Cross-references — names titles (**Alerting strategies**, **Weft functions**); mixes `man alert` and `help cron` (l.27, 59); check both resolve.
- [?] 12. Renders cleanly — `help cron` source contains literal `{{TIER_LIMITS}}` (crons-28); confirm runtime substitution.
- [x] 13. Length proportionate; some overlap with `help cron`.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| crons-06 | 21 | `tail -n 500 myapp.log \| awk ...` | `tail -n 500 /log/<ns>/<env>/<svc>/main.log ...` (bare name gives "unknown source") |
| crons-14 | 28 | "there is no separate cron-level notify field" | reconcile with `help cron`, which documents `# notify: ops` (doc or help is stale) |
| crons-20 | 36-37 | predicates "return booleans" | "exit 0/1 for use in composite alerts" |
| crons-05 | 20 | only `*/5 * * * *` | also mention `# schedule: every 5m` form that `help cron` documents |
| crons-24 | 46 | `1h` with no caveat | note durations like `30m`, `1h`, `6h`; `1d` is rejected |
| crons-34 | 65 | `git restore ...` "rolls back" | it creates a new commit making the old version current |
| crons-35 | 66 | "Five most recent past versions are kept" | say whether the current version is counted |
| crons-28 | 57-59 | tier limits via `help cron` | verify `{{TIER_LIMITS}}` is substituted at runtime |

## Needs your decision first (product vs doc)
- Example pipeline (crons-06): `tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l` always returns 500 (filter applied before tail). Fix planner, or reword example (e.g. `grep -c`).
- `# notify:` directive (crons-14): help documents it, doc denies it. Keep the directive and update this doc, or remove it from help.
- Product error strings say `watch "x": record not found`; rename to cron, or leave as an internal string.

## Notes
- Move the Lua comment (l.48-49) out of the shell fence or give it its own `lua` fence.
- `vi /cron.d/...` and the `:w` commit prompt (crons-03/32) and Dashboard sparklines (crons-10/15) need an interactive/browser check.
- "Cron version history" overlaps the **git** doc; consider a one-line pointer only.

## Applied 2026-10-12
- Example is now `cat /log/<ns>/prod/api/main.log | grep ERROR | wc -l | alert ...` (no -n); added schedule forms (`every <duration>`), the per-interval time-range explanation and the "-n can silently truncate" warning; "return booleans" -> exit 0/1; Lua snippet moved to its own fence; durations incl. d/w and 24h aggregate cap; version history: restore makes a new version, live + five before kept (matches git doc).
- Left: Dashboard sparkline claim, `vi`/`:w` prompt (interactive only).
