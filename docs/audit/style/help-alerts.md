# Style review: docs/shell/help-alerts.md
Verified: ✅ 13 · ❌ 2 · ⚠️ 16 · 🔒 13 · 🖥 4 · ∅ 4   (copy from the ledger; recount the verdict column)
Audience: customer-facing web+shell   Words: ~580

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [x] 1. Opens with what the reader accomplishes, not background — L16-19 define alert directly.
- [x] 2. Task-oriented sentence-case headings, no filler — fine; "Conditions (summary)" is the weakest.
- [ ] 3. Every code block is copy-pasteable and runs as written — L40-43 fails in `sh -c` (help-alerts-17); L63-69 path absent (-28/-29).
- [ ] 4. Realistic names — L106 `s3cret` and L109 `paul@example.com` are personal/odd; L64 `/log/weft/shard/main.log` is internal.
- [ ] 5. Commands/flags in `code` — L60 "-k and -t" not in backticks; L112-118 bold run-on service labels.
- [ ] 6. Sample output matches the verified output — L19 passthrough is false when alert fires (help-alerts-06).
- [ ] 7. No internal vocabulary — L64/68 path contains `shard`; L49-51 repo paths `docs/manual/*.md`.
- [ ] 8. Terminology: cron (not watch) — L116 webhook payload key `watch` (literal; say what it is).
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [?] 10. Voice — L30 "(no keyword) — any" is unclear; bare `alert` needs `--notify` (help-alerts-14).
- [ ] 11. Cross-references — L49-51 name repo paths; use "Alerting strategies" and "Weft functions" titles (help-alerts-21).
- [x] 12. Renders cleanly in both targets (no stray {{ }}, raw &lt;, broken tables/fences)
- [?] 13. Length proportionate; no duplicate — overlaps `man alert` and Alerting strategies; the notify.yaml section is long for a help page.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| help-alerts-06 | 19 | "Stdin always passes through to stdout unchanged (tee behavior)." | "When the condition does not hold, stdin passes through unchanged." (if behavior intended) |
| help-alerts-17 | 40-43 | two-line composite example | say it applies "in a cron body" if that is where it works (untested) |
| help-alerts-14 | 30 | (no keyword) — "any"; fires when stdin is non-empty | `alert --notify=<alias>` (no condition) fires when stdin is non-empty. |
| help-alerts-32 | 77-79 | `vi /etc/notify.yaml` | add: "Editing requires the configmgr group." |
| help-alerts-28/29 | 64, 68 | `/log/weft/shard/main.log` | generic path `/log/<ns>/<env>/<svc>/main.log` |
| help-alerts-21 | 49-51 | `docs/manual/alerting-strategies.md`, `docs/manual/weft-funcs.md` | "Alerting strategies" and "Weft functions" (draft) |
| help-alerts-12/13 | 28-29 | `rising N%` / `falling N%` | `rising <N>%` / `falling <N>%` to match `man alert` |
| help-alerts-40 | 116 | payload keys `v`, `watch`, ... | keep literal `watch`, add "(the cron name)" (draft) |

## Needs your decision first (product vs doc)
- help-alerts-06: alert swallows stdin and aborts the script when it fires. Fix the doc claim, or make it truly tee.
- help-alerts-17: `alert` after a newline is "command not found" in `sh -c`. Doc says "in a cron body", or the shell accepts `alert` mid-script.
- help-alerts-32/33: alice (configmgr) is denied writes to /etc. Token-group lookup bug, or doc must state the group requirement.
- help-alerts-40: webhook key `watch` vs cron. Rename the key (breaking for receivers), or document it as legacy.
- help-alerts-14: bare `alert` errors without `--notify`. Relax the product, or reword the doc.

## Notes
- Ledger flags `.host` here vs `._host` in alerting-strategies/man-alert; unchecked, reconcile.
- L118 "requires SMTP configuration": unclear who configures it for hosted users.
- L120-128: `weft integration slack connect` returned 503 in the test env; `pagerduty add` verified.
- `ls /etc/notify.yaml` says "file does not exist" while `cat` works (product quirk, help-alerts-30).
- Most notify.yaml claims are 🔒/unexercised; mark as unverified when reviewing.

## Applied 2026-10-12
- Passthrough claim fixed; `rising <N>%`; `any` explained; alert-last-only stated; composite recipe now subshell form plus a time-range awk recipe; -n note added; paths made 4-segment with `cat`/`jq -c`; repo paths replaced by doc titles; example secret/email made neutral; webhook `watch` key kept literal, annotated "(the cron name)".
- Left: configmgr requirement for editing /etc/notify.yaml (32/33, unverified), SMTP wording, notify.yaml redaction (decision 15).
