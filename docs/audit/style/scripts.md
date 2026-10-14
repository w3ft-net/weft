# Style review: docs/manual/scripts.md
Verified: ✅ 18 · ❌ 2 · ⚠️ 14 · 🔒 11 · 🖥 4 · ∅ 4
Audience: customer-facing web+shell   Words: ~620

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L10-16 is fine but L14-16 buries validation; the first example appears at L23.
- [ ] 2. Task-oriented sentence-case headings, no filler — "Language support", "Bash", "Lua" are noun headings (L37-59); "Editing a script with `vi`" is task-based.
- [ ] 3. Every code block is copy-pasteable and runs as written — fails: scripts-11/12/45 (bare `api.log`, `disk.log`, L31-32, L103); `less` in L57 does not page.
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold** — "vim" at L14 vs `vi` at L110; `cat - > myscript` fine.
- [?] 6. Sample output matches the verified output — L31 "→ number of ERROR lines" is a comment; real output is a number from a 4-segment path (scripts-11).
- [ ] 7. No internal vocabulary — doc is clean, but L106 "weft cannot optimize Lua scripts" and L35 "weft optimizes these cases" hint at the planner; fine, your call.
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [ ] 9. No stale roadmap language — L118-120 "Watch this if you iterate quickly" is a verb but reads like the obsolete noun; no roadmap hedging found.
- [ ] 10. Voice: 2nd person, present tense, active — L35 "We recommend", L39 "generous subset", L61 "You might reach for" are hedgy; L72 "Weft supplies you with" is stilted.
- [?] 11. Cross-references — **Weft functions** and **git** by title (good); L88 and L114 resolve on web only if titles match (🖥).
- [ ] 12. Renders cleanly in both targets — `io.read("*l" \| "*a" \| "*n")` has escaped pipes in a table cell (L85); stray trailing spaces at L35, L41, L62, L108.
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| scripts-11/12 | 31-32 | `count_errors api.log` / `count_errors disk.log \| wc -l` | `count_errors acme/prod/api/main.log` (relative to `/log`; body stays `cat /log/$1 \| grep ERROR \| wc -l`) |
| scripts-45 | 103 | `cat api.log \| extract_ids \| sort -u \| wc -l` | `cat /log/acme/prod/api/main.log \| extract_ids \| sort -u \| wc -l` |
| scripts-26/27 | 57 | builtins list includes `less` | drop `less` from the list until it pages |
| scripts-17 | 45 | "Effectively the full bash grammar is available" | narrow it: `cat < file`, `command -v`, `type` are not available |
| scripts-32 | 73-74 | "plus a restricted `os` (`os.clock`, `os.time`, `os.difftime`)" | add `os.date` |
| scripts-33 | 74 | "`dofile`, `loadfile`, `load`, and `require` are disabled" | note `loadstring` is still available (or remove it in product) |
| scripts-51 | 115-116 | `git log`, `git show`, `git diff`... | give the path form: `git log -- /bin/<name>` (bare path gives `fatal: bad revision`) |
| scripts-12 | 32 | "`count_errors disk.log \| wc -l` -- scripts compose" | `wc -l` yields 1 on a single number; use `sort`/`xargs` or another stage (draft) |
| scripts-52 | 118-120 | "five most recent past versions are kept" | align with git doc ("latest six versions: the live one plus five") (draft) |
| style | 14 | "Create scripts with vim" | "with `vi`" (draft) |

## Needs your decision first (product vs doc)
- scripts-45/43: a Lua `/bin` script as a stage after a log read fails (`compute exit 127: command not found`). Fix the compute path, or document scripts as stdin-only for Lua.
- scripts-19: `cat < file` fails with "missing pseudofile argument". Fix redirect handling, or document the limit.
- scripts-26/27: `less` is a pass-through noop. Ship a pager, or remove it from the builtin list.
- scripts-33: `loadstring` is available despite the sandbox claim. Remove it in product, or document it.

## Notes
- scripts-34 (30s timeout) and scripts-52 (version cap count) are unverified; the cap wording also conflicts with the git doc (git-doc "six", this doc "five past").
- Error text for unknown crons says "watch" (scripts-42); fix in product, not doc.
- `weft.avg` returns `nil` on unknown cron, which would crash the L69 `%.2f` example; consider a nil guard.
- Seeded `count_errors` uses `awk 'LEVEL == "ERROR"'`; doc example uses `grep ERROR`. Pick one.

## Applied 2026-10-12 (docpass)
- Examples use real path forms (count_errors myco/prod/api/main.log; /log/myco/prod/api/events.log); added a line that a bash or Lua script works as a later stage after a log read (decision 9, verified with severity/status.lua); vim -> `vi`; `git log -- /bin/<name>`; "Watch this" reworded.
- Left: `less` in the builtin list (paging pending); "five most recent past versions" vs git doc "six"; Lua sandbox text already matches decision 12; "weft optimizes" wording; extract_ids example assumes JSON lines (json.decode errors on non-JSON lines).
