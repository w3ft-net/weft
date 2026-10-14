# Style review: docs/manual/notify.md
Verified: ✅ 10 · ❌ 0 · ⚠️ 9 · 🔒 10 · 🖥 3 · ∅ 1   (33 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~320

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes — l.12 states what `--notify` does and how to configure it.
- [ ] 2. Task-oriented sentence-case headings — page title and first H2 both "Notifications" (l.10); "Testing an alias" ok.
- [ ] 3. Every code block copy-pasteable — l.41 `R0UTING_KEY_FROM_PD` (zero) reads as a typo; l.78 sends to real destinations.
- [ ] 4. Realistic names; no real data — placeholder key looks like a typo; use `YOUR_PAGERDUTY_ROUTING_KEY`.
- [x] 5. Commands/flags in `code`, UI in bold — **Account → Integrations** bolded; fine.
- [x] 6. Sample output matches verified output — no sample output shown.
- [x] 7. No internal vocabulary — none.
- [x] 8. Terminology — cron, alias, token consistent.
- [x] 9. No stale roadmap language; no Windows — none.
- [x] 10. Voice — good.
- [?] 11. Cross-references — `help alerts` (l.28) vs `man alert` elsewhere; confirm intended and that web shows an equivalent.
- [x] 12. Renders cleanly — generated ts escapes ok, no stray `{{ }}`.
- [?] 13. Length proportionate — fine; l.12-29 partly duplicates `help alerts` grammar (named as pointer, ok).

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| notify-12 | 33-34 | "the credentials behind them live with your account" | add: "Webhook headers, including any secrets, are written in notify.yaml itself." |
| notify-26 | 59 | heading "Webhook and email need no setup" | "Webhook and email need no account connection" (email still needs SMTP, l.73) |
| notify-30 | 77-79 | `echo "test fire" \| alert --notify=ops` | note it sends to every target in the alias |
| style | 41 | `R0UTING_KEY_FROM_PD` | clearly-placeholder key (draft: `YOUR_ROUTING_KEY`) |
| style | 10 | duplicate H2 "Notifications" under title | drop the redundant H2 or rename (draft: "Define a notification alias") |

## Needs your decision first (product vs doc)
- `weft integration --help` errors "unknown integration command" (notify-13). Add `--help` support, or leave; doc is fine either way.
- Secrets in plaintext webhook headers in notify.yaml (notify-12): keep as a documented caveat, or add a secret-reference mechanism.

## Notes
- Most claims are 🔒 (PagerDuty/Slack/SMTP/write to /etc); delivery and incident behavior (notify-31/32) were verified only from help text.
- Browser claims (Account → Integrations, notify-14/15) need a manual check that the web UI matches CLI.
- Heading hierarchy is flat (no H3); fine for this length.

## Applied 2026-10-12
- Heading renamed "Define a notification alias"; placeholder key; "no account connection" heading; test-alias note that every target is hit.
- Left: webhook-header secret caveat (notify.yaml will show redacted secrets, decision 15), `integration --help`.
