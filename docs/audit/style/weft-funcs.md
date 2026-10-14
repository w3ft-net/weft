# Style review: docs/manual/weft-funcs.md
Verified: ✅ 34 · ❌ 3 · ⚠️ 12 · 🔒 0 · 🖥 2 · ∅ 9
Audience: customer-facing web+shell   Words: ~1070

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L10-19 describes "three shapes" before showing a task; start with one example (L134).
- [ ] 2. Task-oriented sentence-case headings, no filler — noun headings ("Caps", "Numeric parsing", "Retention", "Anomaly guards"); acceptable for reference, your call on rewording.
- [ ] 3. Every code block is copy-pasteable and runs as written — fails: funcs-12 (`weft::previous`, L44), funcs-51/52 (`1d`/`4w`/`1w`/`8w`, L116-121); funcs-26 (`14d`, L64).
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — only one example (L134), no output shown; funcs-55 ran silently.
- [ ] 7. No internal vocabulary — "the same Go implementation backs both" (L29-30); "Go syntax" (L39); "O(1) anchor rows" (L65).
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice: "there's no drift to worry about" (L30) overclaims (Lua and bash disagree, funcs-51); "In Lua you'd just write" (L137) is conditional.
- [ ] 11. Cross-references name a topic — **Crons**, **Alerting strategies**, **Custom scripts**, `man alert` all by title (good); funcs-05 note: `man alert` unchecked.
- [ ] 12. Renders cleanly in both targets — `&lt;2` raw entity in table (L44); escaped `\|` in table cells (L104-105) may leak.
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| funcs-12 | 44 | `weft::previous <w>` listed as a bash builtin | register the builtin, otherwise: "`weft::previous` is Lua-only" |
| funcs-51/52 | 116-121 | `period=1d window=4w`; `period=1w window=8w` | "`period=24h window=672h`" (bash rejects `d`/`w`); also fix prose: four daily samples need a 4d window, not 4w |
| funcs-10/26 | 38-39, 64 | "Durations use Go syntax: `30s`... `14d`" | "Durations are numbers with s/m/h units (no d or w): `30s`, `5m`, `24h`, `336h`; the endpoint cap is 14 days (336h)" |
| funcs-37 | 91-94 | "predicates return **false** rather than erroring" | add "(except `is_absent`, which fires when data is missing)" |
| funcs-45 | 102 | `[<pct>] [<dur>]` independently optional, default unstated | state the default pct; show `weft::is_rising <w> <pct> <dur>`; a window needs a pct |
| funcs-18 | 50 | "Count of runs in window (numeric or not)" | "count of results (rows) in window" |
| funcs-06 | 29-30 | "same Go implementation backs both — there's no drift to worry about" | drop the sentence (Lua accepts what bash rejects, funcs-51) (draft) |
| style | 44 | `(`""` if &lt;2 exist)` | `(`""` if fewer than 2 exist)` (draft) |
| style | 29, 39, 65 | "Go implementation", "Go syntax", "O(1) anchor rows" | remove implementation words (draft) |

## Needs your decision first (product vs doc)
- funcs-12: `weft::previous` missing in bash but present in Lua. Register the bash builtin, or document it as Lua-only.
- funcs-51/52: bash rejects `d`/`w` units; Lua silently returns false even for `period "bogus"`. Accept `d`/`w` and validate in Lua, or document hours only.
- funcs-37: `is_absent` fires on a nonexistent cron. Treat as intended and document the exception, or make it consistent.
- funcs-45: default `pct` is undocumented and unknowable (<= 23.3). Pick and document a default.

## Notes
- funcs-05/55: stdin-comparator examples cannot be observed exit codes via `weft sh -c`; confirm interactively, 🖥.
- Pre-existing crons only; has_changed-constant, is_absent-empty, previous-with-<2, zero-stddev guard untested (∅).
- "See also" is useful; consider moving it up since cross-references to Alerting strategies appear at L32 too.
- The `weft::avg error-rate 5m | weft::is_gt 50` example (L134) is the best opener; consider moving it to the top.

## Applied 2026-10-12 (docpass)
- Removed "same Go implementation... no drift" and "O(1) anchor rows"; `&lt;2` -> "fewer than 2"; count = results (rows); is_rising/is_falling syntax `[<pct> [<dur>]]` (dur needs pct, verified); seasonal example period=1d window=4d. d/w durations and is_absent text were already correct (verified: 1d12h parses, caps 24h/14d, bogus window prints the reason).
- Left: weft::previous is not registered in bash (Lua only) - product decision; default pct for is_rising/is_falling unstated.
