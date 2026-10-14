# Style review: docs/manual/git.md
Verified: ✅ 62 · ❌ 0 · ⚠️ 17 · 🔒 41 · 🖥 9 · ∅ 0
Audience: web only (no shell help entry for `git`)   Words: ~1440

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L12-16 is fine but L18-23 stalls on "subset of git" before any task.
- [ ] 2. Task-oriented sentence-case headings, no filler — "The five-version cap" says five, body says six (L75-78); "What `git` here does NOT do" is shouty.
- [ ] 3. Every code block is copy-pasteable and runs as written — all run; L114 `HEAD~2:bin/scan` needs repo-relative form (git-23/24); L173 hash/log samples are illustrative.
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [ ] 6. Sample output matches the verified output — blame shows `Bob`/`Alice`, real output shows full display names (L145-146); hash widths inconsistent (L93, L145).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice: 2nd person, present tense, active — "We keep each file's latest six" (L77) fine; L21-23 colon dangles into `git --help` line.
- [?] 11. Cross-references name a help/man topic or title — none present; your call whether to point `adduser`/`deluser` (L245) at a man topic.
- [ ] 12. Renders cleanly in both targets — table L44-52 uneven; L160-162 sentence split by a hard-wrapped em-dash.
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| git-26 | 57-59 | "A path outside `/bin` and `/cron.d` is refused: `fatal: '/tmp/x' is outside the repository (only /bin, /cron.d, /etc/dashboards and /etc/notify.yaml are tracked)`." | "A path outside the tracked set is refused: `fatal: '/tmp/x' is outside the repository (only /bin, /cron.d, /etc/dashboards and /etc/notify.yaml are tracked)`." |
| git-40/74 | 82, 149 | "A `^` marks a line that predates what the five-version cap kept." | "A `^` marks a line that comes from the oldest version still kept (the file's first version, or the cap's edge)." |
| git-71/73 | 145-146 | blame sample authors `Bob`, `Alice` | use full display names, e.g. `Dana Okafor`, `Eli Bergstrom` |
| git-23/24 | 56-58 | "Write a path any way git accepts it: `/bin/scan`, `bin/scan`, or just `scan` after `cd /bin`." | add: "(after `<rev>:` use the repo-relative form, `bin/scan`)" |
| git-06/09 | 18-23 | "Everything it accepts behaves and prints the way git does" | add: "Flags it doesn't implement are refused with `fatal: unrecognized argument`; the flag list is under `git log`." |
| git-129 | 3 | title "version history for /bin, /cron.d and /etc" | "version history for scripts, crons, dashboards and alert aliases" (draft) |
| git-119 | 238 | `add`, `rm`, `mv`... "don't exist" | "are refused (`is a git command, but weft doesn't support it`)" (draft) |
| style | 75-78 | heading "The five-version cap" vs "latest six versions" | align the heading and body on one count (draft) |
| style | 93, 145, 160 | hashes `a4fb0c2` / `a4fb0c2f` / `0174c77` | one consistent hash width, and a restore hash that appears in the log sample (draft) |
| style | 21-23 | dangling colon, then `git --help` line, then block | move the `git --help` sentence after the two examples (draft) |

## Needs your decision first (product vs doc)
- git-21: `git log -- /log/...` fails with a planner error, not the git "outside the repository" fatal. Fix product to refuse like other paths, or document.
- git-101: `--color=always` does not colour `git log --oneline`. Fix product, or narrow the doc claim to patch output.
- git-06: `git status --short`, `git log --format=`, `git log --help` are refused. Implement the common flags, or state the limits in the doc.

## Notes
- Many claims (cap, restore, revert, amend, vi popup, permissions) are 🔒/🖥; the sheet cannot confirm them from the ledger.
- "Colour" is British spelling; the rest of the doc is US (L190).
- Doc has no `man git` counterpart; the shell cannot show this page.
- git-54: `--since=1d` is rejected; the doc already lists only supported date forms, so no change needed.

## Applied 2026-10-12
Changed: outside-tracked-set wording; repo-relative after <rev>:; unrecognized-flag note; blame sample full names; add/rm/... refused wording; ^ marker wording. Left: five-vs-six heading, title, hash widths, git log -- /log planner error, --color on --oneline, status --short/--format.
