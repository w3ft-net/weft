# Style review: docs/shell/man-alert.md
Verified: ✅ 23 · ❌ 5 · ⚠️ 21 · 🔒 9 · 🖥 3 · ∅ 0   (recounted from ledger rows)
Audience: shell only (man page; also rendered in web shell help)   Words: ~840

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes, not background
- [?] 2. Task-oriented sentence-case headings, no filler — all-caps man-page headings (l.12-136); keep if that is the man convention.
- [ ] 3. Every code block is copy-pasteable and runs as written — l.118, 122, 126, 130 (ledger 52, 56, 58, 59); l.119-121 need undefined `check_errors` (53-55); l.91-95 cron-only (34).
- [ ] 4. Realistic names; no real customer/account data — `myapp.log`, `/log/myco/canary/main.log`, `/log/weft/shard/main.log` do not resolve (l.118, 122, 126, 130).
- [ ] 5. Commands/flags in `code`, UI elements in **bold** — OPTIONS bullets l.107-113 are very long single lines; split.
- [?] 6. Sample output matches the verified output — no sample output shown; l.31 default `any` behavior differs (08).
- [ ] 7. No internal vocabulary — repo path `docs/manual/alerting-strategies.md` l.102; example path `/log/weft/shard/main.log` l.126, 130 leaks "shard".
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice — l.34 "Always passes stdin through" overstated (09); l.31 "default is any" (08); l.110 "Required" stronger than behavior (43).
- [ ] 11. Cross-references — l.102 repo path; l.136 `vi`, `cat` lack `man`; add `man weft`, `help cron`.
- [x] 12. Renders cleanly in both targets (static check found no stray {{ }} or &lt;)
- [?] 13. Length proportionate; no duplicate — l.102-103 overlaps "Alerting strategies" doc; confirm man page should not restate multi-row grammar twice (l.51-70 and l.109-110).

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| 52 | 118 | `tail -n 500 myapp.log \| awk ... \| alert --notify=ops` | `tail -n 500 /log/myco/prod/api/main.log \| awk 'LEVEL == "ERROR"' \| alert --notify=ops` |
| 56 | 122 | `cat /log/myco/canary/main.log \| tail -1 \| alert absent ...` | use `/log/<ns>/<env>/<svc>/<kind>` path, e.g. `/log/myco/prod/canary/main.log` (draft) |
| 58 | 126 | `tail /log/weft/shard/main.log \| jq -r ...` | `/log/myco/prod/api/metrics.log` (4-segment path) (draft) |
| 59 | 130 | `jq 'select(.type=="disk")' \| alert ... -k .pct -t .host` | `jq -c 'select(.type=="disk")' \| alert above 80 -k .pct -t .host --notify=ops` |
| 09 | 34 | Always passes stdin through to stdout unchanged (tee behavior). | When the condition is not met, stdin is passed to stdout; alert must be the last stage of the pipeline. |
| 08 | 31-32 | default is "any", fires when stdin non-empty | If no condition is given and `--notify` is supplied, the default is `any`. A bare `alert` with neither is a usage error; write `alert any` for a dry run. (only if product stays) |
| 04 | 19-21 | SYNOPSIS omits `--if-zero`/`--if-nonzero` | add `\| alert (--if-zero <v> \| --if-nonzero <v>) [--notify=<alias>]` form |
| 50 | 112 | `--average` for above/below with `--duration` | add "has no effect without `--duration`" |
| 48 | 111 | Examples: 5m, 1h, 30s | add "units s, m, h only" |
| 53-55 | 119-121 | `check_errors` undefined | say it is a user script in /bin, or use a concrete pipeline |
| 38 | 102 | `docs/manual/alerting-strategies.md` | name the help topic (`help alerting-strategies` or equivalent) |
| 60 | 136 | `help`, `help alerts`, `vi`, `cat` | add `man weft`, `help cron`, `/etc/notify.yaml`; use `man vi`/`man cat` if they exist |

## Needs your decision first (product vs doc)
- 08: bare `alert` (no `--notify`) errors with usage. Fix product to fire on `any`, or document the caveat.
- 03: unknown flags (`--foo`) are silently ignored and the shell usage line differs from SYNOPSIS (shows --notify required). Product fix or doc only?
- 56: `alert absent` fires falsely when the upstream query fails (`unknown source`). Product footgun; doc cannot fix it.
- 43: `-k` without `-t` does not error in the shell; `-t` alone ignores the condition. Enforce "required" in product, or soften l.110.
- 07: doc names Slack and PagerDuty; ledger saw only webhook and email keys in `/etc/notify.yaml`. Confirm supported targets.

## Notes
- COMPOSITE PREDICATES l.86-103 may belong in the alerting strategies doc, leaving a one-line pointer here.
- l.81-82 "previous/window average" is vague (30); define the baseline precisely.
- Multi-row field grammar is stated twice (l.61-70 and l.109-110); consolidate.
- Ledger 🔒 items (dedup, incident close, `--duration` windows) were not runnable; wording unverified.

## Applied 2026-10-12
- Examples rewritten to valid 4-segment paths with `cat` (no `-n`, no `tail`-fed multi-row), `jq -c`; `check_errors` marked as a user /bin script; `alert absent` recipe has no `tail -1`.
- Passthrough claim fixed (only when the condition is not met); default `any` documented as needing `--notify` (bare `alert` is a usage error); alert-last-only stated; `--if-zero` synopsis form added; `--duration` units s/m/h; `--average` needs `--duration`.
- Composite recipe replaced by subshell form `(... && echo fire) | alert`; repo path replaced by `man alerting-strategies`; SEE ALSO extended (`man weft-funcs`, `help cron`, `man range`).
- Left: Slack/PagerDuty target list (07), `-k` without `-t` enforcement (43), unknown flags silently accepted (03), `alert absent` firing on a failed upstream query (56), vague rising/falling baseline wording.
