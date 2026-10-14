# Style review: docs/manual/alerting-strategies.md
Verified: ✅ 20 · ❌ 10 · ⚠️ 20 · 🔒 14 · 🖥 6 · ∅ 3   (73 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~1280

## Pre-filled checklist
- [?] 1. Opens with what the reader accomplishes, not background — l.10-15 is a catalogue intro, ends "This page is about which shape fits"; fine for a reference page.
- [ ] 2. Task-oriented sentence-case headings, no filler — "The four shapes" (l.17) vs intro listing three (l.10-12); "Anti-patterns" ok.
- [ ] 3. Every code block copy-pasteable — fails: -07 (l.27), -11/12 (l.43), -18 (l.57), -22/23 (l.77-97, 148-151, 208-211), -66 (l.229).
- [ ] 4. Realistic names; no real customer data — `/log/myco/api/...` and `/log/weft/shard/main.log` (l.27, 43, 57) are not valid paths; alias `critical` undefined (l.81).
- [x] 5. Commands/flags in `code`, UI in bold — consistent.
- [ ] 6. Sample output matches verified output — l.178 says missing cron returns empty string; actual is an error (-52).
- [ ] 7. No internal vocabulary — `/log/weft/shard/...` (l.43) leaks internal shard naming; no warp/DuckDB otherwise.
- [x] 8. Terminology: cron, not watch — consistent (product errors still say "watch", see Notes).
- [?] 9. No stale roadmap language; no Windows — none found; "14d" cap (l.153) promises an unreachable window.
- [x] 10. Voice: 2nd person, present, active — mostly fine.
- [ ] 11. Cross-references — l.244 names repo path `docs/manual/notify.md`; use **Notifications**.
- [?] 12. Renders cleanly in both targets — l.72-73 "error-\nrate" hyphen break may render "error- rate"; generated ts otherwise clean.
- [?] 13. Length proportionate — long (244 lines); composite section (l.70-131) overlaps `man alert`/help alerts examples.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| -07 | 27 | `/log/myco/api/main.log` (3-seg) and `grep ERROR \| wc -l` | `/log/myco/prod/api/main.log`; or `grep -c ERROR \| alert above 50` until the planner bug is fixed |
| -11/12 | 43, 57 | `/log/weft/shard/main.log` | a real path, e.g. `/log/myco/prod/api/metrics.log` |
| -18/20 | 57-68 | plain `jq` with `-k .pct -t ._host`; `._deployment/._pod/._region` | `jq -c 'select(.type=="memory")' \| alert above 80 -k .pct -t .host`; drop `._pod`/`._region` or state which dimensions exist |
| -21 | 67 | `"\(._region) \(._host) \(.pct)"` gives region-prefixed host names | `-t 1` takes the first token only; build one token: `"\(._region)/\(.host) \(.pct)"` |
| -43/38 | 153 | 14d cap for endpoint funcs | `1d`/`14d` are rejected ("use e.g., 30m, 1h, 6h"); state 24h cap only, or fix product (see below) |
| -52 | 178 | `weft::latest` of a missing cron returns the empty string | in bash it is an error (guard with `weft::is_absent`); Lua behavior (draft: verify) |
| -66 | 229 | `weft::is_above ... && curl https://my-deploy-bot/...` | generic side-effect example; curl is not in the shell |
| -48-50 | 166-168 | `is_rising`/`is_outlier`/`is_seasonal` with no args; N% | add signatures: `is_rising <cron> <pct> <window>` (bare number), `is_outlier <cron> <n_stddev> <window>`, `is_seasonal <cron> <n_stddev> <period> <window>` |
| -63/23 | 81 | `--notify=critical` | note you must define the `critical` alias first (see Notifications) |
| -04 | 10-17 | intro lists three shapes; heading says four | list all four incl. aggregated-value |
| -73 | 244 | `docs/manual/notify.md` | **Notifications** |
| -06 | 25 | `# schedule: every 1m` | unconfirmed (🔒); existing crons use `*/1 * * * *`; confirm `every 1m` accepted |

## Needs your decision first (product vs doc)
- Composite recipes (-22/23/28/61; l.77-97, 148-151, 208-211): `alert` as second statement gives "command not found". Fix product to allow it, or rewrite recipes to one line.
- `tail -n 500 | grep ERROR | wc -l` returns 500 (-07): planner pushdown reorders filter before tail. Fix planner, or change the headline recipe.
- `1d`/`14d` durations rejected (-43): add day units to `weft::*`, or drop the 14d cap sentence.
- `._host` etc. (-18/20): expose underscore dimensions in raw JSON rows, or document `.host`/`.deployment` as the tag fields.
- `weft::latest` on a missing cron errors (-52): make it return empty (matches doc and Lua guard advice), or update doc.

## Notes
- Lua cron examples (l.103-131) show no way to run Lua into `| alert above 0` (l.114 is prose only); either show the syntax or drop that sentence.
- Untestable here (🖥/🔒): Lua semantics, `X | weft::is_gt N` exit codes, `--duration` behavior; re-check interactively.
- Product error strings still say `watch "x": record not found`; the doc is clean but users will see the old term.
- Section order: "Trend / anomaly vs threshold" table could move earlier; the gotcha and debounce sections are fine as is.

## Applied 2026-10-12 (docs/manual/alerting-strategies.md; uncommitted, generated files not regenerated)
- Intro now lists all four shapes (-04) and explains `--notify=<alias>` / `/etc/notify.yaml` (-63/23: `critical` and `ops` are aliases you define).
- Paths: the headline recipe's `/log/myco/api/main.log` is valid syntax and is unchanged (-07); `/log/myco/api/metrics.log` replaces `/log/weft/shard/main.log` (-11/12), because disk rows live in metrics.log.
- Tag fields: `._host` -> `.host`; `jq` -> `jq -c` in the JSON-path form; dropped the `._pod`/`._region` claim; tag combining example uses one token `"\(.deployment)/\(.host) \(.pct)"` (-18/20/21). Re-run against live rows before editing.
- Added the `is_rising` / `is_outlier` / `is_seasonal` signatures, bare-number pct, h/m/s units (-48-50). Re-run before editing: `50%` rejected, `1d` period rejected.
- Removed the `curl` example from the side-effects anti-pattern (-66). Hyphen-wrapped `error-\nrate` fixed (-12 style). See-also now says **Notifications** (-73).
- NOT changed (waiting on you): composite recipes needing `alert` as a second statement (-22/23/28/61); `tail | grep | wc -l` filter ordering (-07 behavior); 14d window cap (-43/38); `weft::latest` on a missing cron (-52); `# schedule: every 1m` (-06, unconfirmed).
- Render check: `docgen` into a scratch dir changes only `alerting-strategies.ts` and one corpus const; no `{{ }}` placeholders. Repo generated files untouched.

## Applied 2026-10-12 (second pass)
- Decisions 1/4/5/14: every cron recipe is -n-free and reads its own interval; `tail` replaced by `cat`; composite section rewritten (time-range awk over several sources, then subshell predicate form `( ... && echo fire) | alert`, no two-statement `--if-zero "$?"` recipes); Lua composite moved to a `/bin` script (crons are bash); debounce recipe uses the scalar subshell form; durations d/w; gotcha section says bash `weft::latest` errors on a missing cron and uses `weft.is_absent` guard in Lua.
- Left: `weft::latest` Lua behavior on a missing cron (unverified), `alert absent` on failed upstream (product).
