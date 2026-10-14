# Style review: docs/shell/help-cron.md
Verified: ✅ 14 · ❌ 2 · ⚠️ 8 · 🔒 6 · 🖥 7 · ∅ 0   (copy from the ledger; recount the verdict column)
Audience: shell only   Words: ~190

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [x] 1. Opens with what the reader accomplishes, not background — L12 defines a cron in one sentence, then lists commands.
- [?] 2. Task-oriented sentence-case headings, no filler — "Cron file format", "Accessing past results" fine; "Tier limits" is a noun heading.
- [ ] 3. Every code block is copy-pasteable and runs as written — L27 path fails (hcron-19); L25 header spellings unverified (hcron-15/16).
- [ ] 4. Realistic names; no real customer/account data — L27 `/log/myco/api/main.log` is not a valid path; L39 `error-rate` is never defined (hcron-34).
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — no output shown; L18 sparkline column not seen under `sh -c` (hcron-10).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token — prose is clean; product usage strings still say `<watch-name>` (hcron-26).
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice — L32-34 "read this cron's history" is wrong: the functions need an explicit cron name (hcron-26).
- [ ] 11. Cross-references — L36 `man weft` is not in the `help` index list; L14/19 refs are fine.
- [?] 12. Renders cleanly in both targets — L44 `{{TIER_LIMITS}}` is expanded at runtime in the shell only; no web page uses this doc (hcron-36).
- [x] 13. Length proportionate; no duplicate of another doc — overlaps `man crons` lightly.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| hcron-19 | 27 | `cat /log/myco/api/main.log \| grep ERROR \| wc -l` | `cat /log/myco/prod/api/main.log \| grep ERROR \| wc -l` |
| hcron-26 | 32-34 | "read this cron's history" | "read a cron's history; pass the cron name, e.g. `weft::latest error-rate`." |
| hcron-16 | 25 | `# schedule: every 1m          (or cron: */5 * * * *)` | put `# schedule: */5 * * * *` as a separate example once verified (ledger) |
| hcron-15 | 25 | `# schedule: every 1m` | unverified; existing crons use `*/1 * * * *`; verify before keeping |
| hcron-17 | 26 | `# notify: ops` header | unverified header; no existing cron uses it; verify before keeping |
| hcron-34 | 39 | `weft::avg error-rate 1h` with no cron named error-rate | say the name must match your cron (draft) |

## Needs your decision first (product vs doc)
- hcron-26: functions take a required cron name. Keep the explicit name and fix the doc, or make the name implicit inside a running cron (untested).
- hcron-26 / hidx-11: usage strings say `<watch-name>`/`<watch>`. Product strings should be renamed to cron.
- hcron-10 (L18): `ls -l /cron.d/` shows no sparkline under `sh -c`. Likely TTY-only; confirm interactively, else drop the claim.
- hcron-03/04: stdout-as-result and stderr-as-alert are only indirectly observed (cron writes were blocked); confirm with a real cron.

## Notes
- L25 mixes a comment and an alternative spelling inside one code block; split into two examples.
- L14 and L19 point to `help alerts` and `man chart`, both fine.
- L39 example is not the cron body from L27; consider one running example throughout.
- `weft::p95` and `weft::median` do not exist; "and the rest of the family" (L33) is safe.
- Ledger says the web side has no page generated from this doc.

## Applied 2026-10-12
- Removed the `# notify:` header and the `cron:` spelling (neither is parsed; ParseCron only reads `# schedule:`); notification is `alert --notify=`. Path fixed to 4-segment; example pipeline gets an alert stage; -n warning; history functions need a cron name; `man weft` -> `man weft-funcs`.
- Left: `ls -l /cron.d/` sparkline claim (TTY only), `{{TIER_LIMITS}}` placeholder (generated at runtime).
