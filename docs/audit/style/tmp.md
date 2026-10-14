# Style review: docs/manual/tmp.md
Verified: ✅ 14 · ❌ 9 · ⚠️ 16 · 🔒 11 · 🖥 2 · ∅ 3   (55 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~870

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes — l.12-16 says what /tmp is for.
- [ ] 2. Task-oriented sentence-case headings — H2 duplicates the page title (l.10); "What /tmp is not" ok.
- [ ] 3. Every code block copy-pasteable — fails: tmp-03/06/12 (bare `api.log`, l.20/27/31/37), -07/08/10/18 (mkdir, l.30-33, 47), -15 (join, l.40), -17 (touch, l.44), -38 (l.97).
- [ ] 4. Realistic names — bare `api.log`, `orders.csv prices.csv` not in /tmp; `/log/myco/api/main.log` is 3-seg (l.97).
- [x] 5. Commands in `code`, UI bold — consistent.
- [ ] 6. Sample output matches — `ls -lh` sample (l.132-136) omits the owner column (tmp-51/52).
- [?] 7. No internal vocabulary — "binmgr"/"script validation" (l.119-122) edges toward internal; "streaming query" ok.
- [x] 8. Terminology — "account" vs "org" mixed (l.12, 99); cron fine.
- [ ] 9. No stale roadmap language — "not supported in this release" (l.98) is a stale hedge; no Windows.
- [ ] 10. Voice — l.52 "Despite the shell rules" unclear; l.80 "clear error" is filler.
- [?] 11. Cross-references — **Crons** section / **Custom scripts** section (l.66, 94) are unlinked bold; name doc titles.
- [?] 12. Renders cleanly — static check clean; tables fine.
- [?] 13. Length proportionate — long; command table (l.103-117) overlaps built-in command help.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| tmp-07/08/10 | 29-33, 47 | `mkdir /tmp/incident-...` and files inside | flat names: `... > /tmp/incident-2026-04-22-trace`; drop `rm -rf` line; or say "/tmp is flat; no subdirectories" |
| tmp-03/06/12 | 20, 27, 31, 37 | bare `api.log` | `/log/<ns>/<env>/<svc>/main.log`; verify the `$STATUS >= 500` field form (matched nothing) |
| tmp-15 | 40 | `join orders.csv prices.csv` | `join /tmp/orders /tmp/prices` (key is column 1 on both sides) |
| tmp-17/23/25 | 44, 54-60, 112 | `touch` resets the 48h clock | drop `touch` from clock resetters and table row if intended (see decision) |
| tmp-14/43 | 39, 114 | `comm` on raw log output | `comm` needs sorted input; add `sort` or note at l.39 |
| tmp-35 | 83-87 | `cat /etc/usage` shows /tmp usage | shows ingest quota; drop claim or say sizes via `ls -l /tmp` |
| tmp-38 | 95-98 | `tail -f ... > /tmp/stream` "not supported in this release" | see decision; use `/log/myco/prod/api/main.log`; drop "in this release" |
| tmp-47 | 119-120 | cp/mv "restricted to /tmp on both sides" | writes into /bin via cp/mv are refused; `cp /bin/x /tmp/y` works (draft) |
| tmp-51/52 | 126-136 | `ls -l` sample, no owner column | add owner column to sample |
| tmp-46 | 117 | `wc -l a` row lists lines/words/bytes/... | row `wc a` (flags `-l -w -c -m -L` combine) |
| tmp-29 | 73 | Trial 500 KB total, 512 KB per file | per-file limit exceeds total; confirm numbers |

## Needs your decision first (product vs doc)
- `mkdir`/subdirectories (tmp-07): implement directories in /tmp, or rewrite the incident example flat and say "flat namespace".
- `touch` no-op on existing files (tmp-17/25): fix product so it updates mtime (doc correct), or remove `touch` as a keep-alive from doc.
- `tail -f > /tmp/x` silently succeeds with truncated data (tmp-38): reject with an error (doc correct), or document actual behavior.
- Plan names/limits (tmp-28/29): `whoami` says yard, `/etc/usage` says trial; billing owner confirms table.
- `cp /bin/x /tmp/y` allowed (tmp-47): block it to match doc, or reword doc.

## Notes
- l.52 "Despite the shell rules, /tmp is durable storage": rewrite plainly ("/tmp persists across logouts and sessions").
- 48h expiry and cross-account isolation (tmp-19/22/41) were not testable; need time or a second org.
- Page title and H2 both read "Saving results to /tmp"; keep one.
- `ls -l` tail section (l.124-140) may belong with a general `ls` doc.

## Applied 2026-10-12 (docs/manual/tmp.md; uncommitted, generated files not regenerated)
- Paths: bare `api.log` -> `/log/myco/api/main.log` (3-segment, the canonical env-less form) in four examples (tmp-03/06/12). The `tail -f` example already used that form and is unchanged. `api.log` is not a typo: a bare `<name>.log` means the namespace wildcard, so it would have meant namespace `api`.
- `awk '$STATUS >= 500'` (matched nothing) -> `awk '$6 ~ /status=5/'` (tmp-12). Verified: 49 of 200 rows. Note `awk '/status=5/'` and `grep 'status=5' | wc -l` after `tail -n 200` return 200 (filter-before-tail bug), so the field form is used on purpose.
- `comm` now sorts first (it errors "not in sorted order" on unsorted input) and `join orders.csv prices.csv` became a runnable two-file example in /tmp (tmp-14/15/43). Verified live.
- `/etc/usage` claim replaced: it reports ingest quota, not /tmp (tmp-35); usage via `ls -lh /tmp` (tmp-35/51).
- "Despite the shell rules" reworded; "clear error" -> "error"; **Crons**/**Custom scripts** cross-refs no longer say "section".
- `wc -l a` table row -> `wc a` with flags (tmp-46); `ls -lh` sample gains the owner column (tmp-51/52).
- NOT changed (waiting on you): `mkdir`/subdirectories (the incident example still uses mkdir and `rm -rf`), `touch` as keep-alive (l.44, 54-60, 112), `tail -f > /tmp/x` "not supported in this release" wording, `cp/mv` restricted-to-/tmp claim, plan table (Trial 500 KB < 512 KB per-file; yard vs trial naming), "account" vs "org".
- Render check: scratch `docgen` run differs only in alerting-strategies.ts and tmp.ts; no `{{ }}`. Live checks used the dev stack after it was restarted (see below).

## Applied 2026-10-12 (docpass)
- Decision 8: mkdir/rm -r/touch/cp/mv text now true as written (verified live); tail -f > /tmp now an error (verified), bullet rewritten; plan table removed (decision 13) -> "see the Account page"; comments on the tail -n examples say "among the last N rows" (decision 1).
- Left: "account" vs "org" wording; per-file/total limits are no longer stated.
