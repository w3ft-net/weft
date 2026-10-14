# Style review: docs/shell/help-index.md
Verified: ✅ 12 · ❌ 0 · ⚠️ 7 · 🔒 0 · 🖥 2 · ∅ 0   (copy from the ledger; recount the verdict column)
Audience: shell only   Words: ~150

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [?] 1. Opens with what the reader accomplishes, not background — L12 `> weft shell` banner, then a topic list; fine for an index.
- [?] 2. Task-oriented sentence-case headings, no filler — "Topics"/"Reference pages" are noun headings; acceptable for an index.
- [x] 3. Every code block is copy-pasteable and runs as written — no code blocks; all listed commands resolve (hidx-21).
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [x] 6. Sample output matches the verified output — no sample output.
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [x] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
- [ ] 11. Cross-references resolve — L22 says `man` lists "every" topic; it omits 7 pages (hidx-06).
- [ ] 12. Renders cleanly in both targets — L23 blank line splits one list into two groups with no heading.
- [?] 13. Length proportionate; no duplicate — L22 names 3 man topics, L24-30 name 7 others; split is arbitrary (hidx-20).

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| hidx-06 | 22 | `man` — list every manual topic (`man git`, `man crons`, `man metavars`, ...) | `man` — list the manual topics (`man git`, `man crons`, `man metavars`, ...) |
| hidx-14 | 28 | `man range` — set the query time window | `man range` — show or set the query time window |
| hidx-13 | 27 | `man dashboard` — dashboard YAML grammar and tile kinds | align with man page NAME ("view a named tile layout"); verify body first |

## Needs your decision first (product vs doc)
- hidx-06: `man` index omits alert, chart, clipboard, dashboard, less, passwd, range. Fix the doc wording, or add those pages to the `man` index.
- hidx-11 (L25): `chart --help` usage still says `chart <watch>`. Product string is stale; the doc is right (cron).
- hidx-15 (L29): `man less` advertises a pager that is a pass-through under `sh -c` and known broken on the CLI. Keep the line or drop it until paging works.

## Notes
- hidx-05 (L18, `help alerts` covers notify.yaml and dispatching) was retracted: the generated help text does contain both, so no change is needed to that line.
- List order is not alphabetical (`range` before `less`, L28-29); consider sorting.
- The trailing Tip (L32) and the L12 banner both use `>` blockquotes; the banner looks like a quote.
- Working tree shows this file modified; treat ledger line numbers as possibly shifted.
- Ledger compliance incident (adduser/deluser --help probe) is unrelated to style but a human should confirm no stray invite email.

## Applied 2026-10-12 (docpass)
- "list every manual topic" -> "list the manual topics"; stray blank line removed; man range "show or set".
- Left: help cron "tier limits" wording; Tip about auto-paging and `man less` line (paging pending); list order.
