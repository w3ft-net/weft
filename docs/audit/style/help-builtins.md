# Style review: docs/shell/help-builtins.md
Verified: ✅ 34 · ❌ 3 · ⚠️ 19 · 🔒 14 · 🖥 13 · ∅ 1   (copy from the ledger; recount the verdict column)
Audience: customer-facing web+shell   Words: ~690

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [?] 1. Opens with what the reader accomplishes — jumps straight to "Querying logs" (L14); no intro, which suits a reference.
- [x] 2. Task-oriented sentence-case headings, no filler — "Team (owners)" is fine.
- [ ] 3. Every code block is copy-pasteable and runs as written — no fenced blocks, but L16-18 `/log/<app>.log` fails (hb-03/04).
- [ ] 4. Realistic names — L16-18 `<app>` is not a real path form; 1-segment form does not exist.
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — L37 sparkline not seen (hb-22); L40 `-x`/`-y` args misdescribed (hb-26).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology — L39 "watch"/`<watch>`; L77-88 "owners" vs role `admin` in `id`/passwd (hb-71); `nsmgr` group undocumented.
- [ ] 9. No stale roadmap language — none, but L33 advertises `less` paging that is a no-op (hb-18).
- [x] 10. Voice: 2nd person, present tense, active; no hedging
- [ ] 11. Cross-references — L33/40/75/88/92 point to man pages; unverifiable non-interactively (hb-82), L103 claim of per-entry pages unchecked.
- [x] 12. Renders cleanly in both targets (no stray {{ }}, raw &lt;, broken tables/fences)
- [ ] 13. Length proportionate — `paste` (L27, L54) and `wc` (L29, L55) listed twice; git section duplicates `man git`.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| hb-03/04 | 16-18 | `cat /log/<app>.log` (and head/tail) | `cat /log/<ns>/<env>/<svc>/main.log` |
| hb-24 | 39 | `cat /cron.d/<watch>` — view a watch definition | `cat /cron.d/<name>` — view a cron definition |
| hb-26 | 40 | `chart [-x ... -y ...]` | `chart [-x COL\|.path] [-y COL\|.path]` |
| hb-45 | 59 | git over `/bin`, `/cron.d` and `/etc` | "/bin, /cron.d, /etc/dashboards and /etc/notify.yaml" (match L70) |
| hb-48 | 62 | `git show <rev>[:<path>]` | state the path form: `HEAD:bin/x`, no leading slash (draft) |
| hb-76 | 96 | `range` — show or set the query window | `range [<start>..<end>]`, e.g. `range 2h..` |
| hb-73 | 92 | `<pipeline> \| alert <condition>` | add one example: `... \| alert above 200 --notify=ops` (draft) |
| hb-42 | 53 | `join ... (-t / -1 / -2)` | show spaced form `-t ,` (attached `-t,` rejected) |
| hb-65 | 81 | "`deluser` to remove" | `deluser <email> <group>` removes from group; `deluser <email>` removes from org (draft) |
| hb-57 | 71 | "Other git commands are refused" | `git status` works; list it or reword (draft) |
| hb-71 | 77-88 | "owners" | "admins"/owner role named as `id` shows it; add `nsmgr` |

## Needs your decision first (product vs doc)
- hb-18 (L33): `less` is a pass-through. Fix the pager, or drop the line until it pages.
- hb-30 (L42): `mkdir /tmp/<dir>` is "not supported by this filesystem". Implement it, or delete L42 (and `rm -rf <dir>`, hb-37).
- hb-04: `head -n 3 F | tail -n 1` returns the wrong row (planner bug). Product fix; no doc change.
- hb-22 (L37): `ls -l /cron.d/` shows no sparkline under `sh -c`. Confirm on a TTY, else drop it.
- hb-48: `git show HEAD:/bin/x` fails. Accept leading slash in product, or document no-slash form.
- hb-42: `join -t,` rejected. Fix parser (POSIX style) or document spaced form.

## Notes
- L59 vs L70 disagree on what git tracks; unify in one sentence.
- L44 "48h clock" is unexplained; link to `man tmp` (hb-35).
- L19 "list available apps" lists namespaces; reword to "browse the namespace tree".
- L103 tip promises a man page "where one exists"; only checkable interactively.
- `history`, `clear`, `exit`, `vi`, `help`, `man` are interactive-only (🖥); a human must verify.

## Applied 2026-10-12 (docpass)
- Paths now /log/<ns>/<env>/<svc>/main.log with the 3-segment note; ls /log/<ns>/<svc> lists kinds; watch -> cron; chart args; cp/mv both-sides rule and touch/clock; git tracked set unified, git show path form, git status; Team (admins), nsmgr, deluser <email> <group>; alert example with --notify and last-command rule; range form; join -t spacing.
- Left: `less` line (paging pending); "last 5 past versions"; history/clear/exit/man/help (interactive-only); sparkline claim on ls -l /cron.d (needs TTY).
