# Ledger: docs/manual/notify.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|
| notify-01 | 12 | When an `alert` fires in a cron, `--notify=<alias>` dispatches to a destination defined in `/etc/notify.yaml` | default | shell: create cron with `alert ... --notify=<alias>`, force fire, observe incident/notification | ⚠️ | Read-only: `cat /etc/notify.yaml` shows live aliases `ops`, `oncall` with webhook+email; dispatch itself not run (forbidden). Static: help alerts says "--notify=<alias> looks up a destination from your notify.yaml". Human: in a cron run `alert --notify=ops` and watch the target. |
| notify-02 | 13 | The alias is just a name | default | unverifiable (semantic) | ∅ | semantic |
| notify-03 | 14 | Alias fans out to one or more targets | default | shell: alias with 2 targets, fire alert, confirm both | ⚠️ | Live /etc/notify.yaml alias `ops` has webhook + 2 emails (config supports fan-out); actual dispatch to both not observed (no --notify allowed). Human: fire alias `ops` and confirm all targets. |
| notify-04 | 14 | Four service types: Slack, PagerDuty, webhook, email | number | `help alerts` lists four target types | ✅ | static: help alerts: "slack:, pagerduty:, webhook:, and email:. All are lists" |
| notify-05 | 18 | Config file path `/etc/notify.yaml` | path | shell: `cat /etc/notify.yaml` | ✅ | `sh -c "cat /etc/notify.yaml"` printed the file; `ls /etc` lists notify.yaml |
| notify-06 | 19-25 | YAML shape: alias -> {slack: [..], pagerduty: [..], email: [..]} accepted | command | shell: `vi /etc/notify.yaml`, paste, save; confirm no validation error | 🔒 | Needs write to /etc. Step: `weft sh`, `vi /etc/notify.yaml`, paste the doc block (needs configmgr), save, expect no validation error. Read-only: live file parses with same shape (alias -> webhook/email lists), no slack/pagerduty example present. |
| notify-07 | 21 | Slack targets written as `"#alerts"` channel names | default | needs-human (Slack workspace) | ✅ | static: help alerts: `Slack — channel names ("#ops") or {channel: "#name"}` (doc form accepted per help; delivery 🔒 needs Slack) |
| notify-08 | 23 | PagerDuty target is a registered integration name (`ops-daytime`) | default | needs-human | ⚠️ | static: help alerts: "PagerDuty — integration names ... PagerDuty integration name must match one you've registered". Matches doc; delivery 🔒 (PagerDuty). |
| notify-09 | 25 | Email target is a plain address | default | needs-human (SMTP) | ⚠️ | static: help alerts: "Email — bare addresses, or {address: ...}"; delivery 🔒 (SMTP). |
| notify-10 | 28 | Edit with `vi /etc/notify.yaml` | command | shell: `vi /etc/notify.yaml` opens editor | 🖥 | `weft sh` then `vi /etc/notify.yaml` (non-interactive `sh -c` cannot run an editor; write is also 🔒). Static: help alerts also says "vi /etc/notify.yaml". |
| notify-11 | 28-29 | `help alerts` documents full grammar (severities, webhook headers, per-target mappings) | xref | shell: `help alerts`; confirm sections on severities, webhook headers, per-target mappings | ⚠️ | static: help alerts has severities ("severity: critical", critical/error/warning/info), webhook headers ({url, headers}), per-target mappings. Topic `alerts` exists. `help alerts` itself not runnable under sh -c (🖥: type `help alerts`). |
| notify-12 | 33-34 | notify.yaml only names destinations; credentials live with account | default | unverifiable / `weft integration list` shows creds separately | ⚠️ | `weft integration list` shows Slack/PagerDuty state separately from /etc/notify.yaml (consistent), but note live notify.yaml carries a plaintext `X-Signing-Secret` header under `oncall`, so credentials can live in notify.yaml for webhooks; claim is about Slack/PagerDuty only. |
| notify-13 | 34-36 | `weft integration` CLI subcommand exists | command | `weft integration --help` | ✅ | `weft integration` -> "usage: weft integration {list|slack|pagerduty}". Caveat: `weft integration --help` is rejected ("unknown integration command: --help"), usage only printed on bare command. |
| notify-14 | 35-36 | Same functionality under **Account -> Integrations** in browser | UI | web UI: open Account, find Integrations | 🖥 | Open the web UI, Account menu, look for Integrations page. |
| notify-15 | 36 | CLI and browser surfaces do the same thing | UI | compare: add via CLI, see in browser | 🖥 | Add a PagerDuty name with CLI (🔒 write), reload Account -> Integrations in browser and confirm it appears. |
| notify-16 | 39-41 | `weft integration pagerduty add ops-daytime --key R0UTING_KEY_FROM_PD` registers name with key | command | needs-human (PagerDuty key); verify with `weft integration list` | 🔒 | Writes account state. Step: `weft integration pagerduty add ops-daytime --key <real key>` then `weft integration list`. Read-only: `weft integration pagerduty` -> "usage: weft integration pagerduty {add|rm} ..."; list hint shows `weft integration pagerduty add <name> --key <key>` (matches doc form). |
| notify-17 | 40 | The name is what notify.yaml references under `pagerduty:` | default | needs-human | 🔒 | Step: add name then reference it under `pagerduty:` and fire; static help alerts says name must match. |
| notify-18 | 42 | `weft integration pagerduty rm ops-daytime` removes it | command | `weft integration pagerduty rm <name>` then `weft integration list` | 🔒 | Writes account state. Step: `weft integration pagerduty rm ops-daytime` then `weft integration list`. Subcommand `rm` confirmed in usage output. |
| notify-19 | 44-46 | Slack uses OAuth; connect opens a browser tab (like `weft login`) | UI | needs-human (Slack) | 🔒 | Slack OAuth; step: `weft integration slack connect`, browser tab opens, grant access. |
| notify-20 | 46 | CLI waits and confirms once workspace connected | output | needs-human | 🔒 | Step: run connect, confirm CLI waits then prints confirmation. |
| notify-21 | 47 | `weft integration slack connect` | command | needs-human | ✅ | `weft integration slack` -> "usage: weft integration slack {connect|rm}" (subcommand exists; running it is 🔒) |
| notify-22 | 48 | `weft integration slack rm` (no name arg) disconnects | command | needs-human; or `weft integration slack rm` and check list | ✅ | usage "slack {connect|rm}" confirms `rm` takes no name in the usage line; running it is 🔒 (disconnect). |
| notify-23 | 50-51 | `weft integration list` shows connected integrations plus PagerDuty names notify.yaml can use | output | `weft integration list` | ✅ | `weft integration list` -> "Slack: not connected — `weft integration slack connect`" and "PagerDuty: no integrations — `weft integration pagerduty add <name> --key <key>`" (empty state; the "names notify.yaml can use" part not observable with none registered). |
| notify-24 | 54-55 | PagerDuty alias in notify.yaml must match a registered name | default | needs-human: use unregistered name, observe error | 🔒 | Step: put unregistered pagerduty name in notify.yaml, fire alert, observe error. Static help alerts agrees. |
| notify-25 | 55-57 | Slack channels named directly in notify.yaml once workspace connected | default | needs-human | 🔒 | Needs a connected Slack workspace. |
| notify-26 | 59 | Webhook and email need no setup | default | needs-human | ⚠️ | static: help alerts says "Webhook and email destinations need no account-level credentials"; email still needs SMTP (doc says so at line 73). Heading "no setup" slightly overstates, see Findings. |
| notify-27 | 61-62 | Webhook URLs/emails have no account-level credential, written per alias in notify.yaml | default | needs-human | ✅ | static: help alerts same statement; live /etc/notify.yaml carries webhook URLs/emails per alias. |
| notify-28 | 65-71 | YAML shape: alias `pages` with webhook and email lists is valid | command | shell: write to `/etc/notify.yaml` and save | 🔒 | Needs write to /etc. Step: `vi /etc/notify.yaml`, add `pages:` block, save. Read-only: same shape (webhook+email lists) already in live file under `ops`. |
| notify-29 | 73 | Email delivery requires SMTP configured for your deployment | default | needs-human | ✅ | static: help alerts: "email with the cron name as subject (requires SMTP configuration)" ; real send 🔒. |
| notify-30 | 77-79 | `echo "test fire" \| alert --notify=ops` tests an alias | command | shell: run it; observe dispatch | 🔒 | Forbidden this run (would POST to third-party webhook/email). Note: live alias `ops` points at webhook https://hooks.loom.test/weft/ops and emails, so the doc example hits real destinations. Step: `echo "test fire" | alert --notify=ops` in `weft sh`. |
| notify-31 | 81 | Interactive runs dispatch immediately | default | shell: run notify-30, confirm delivery without delay | ⚠️ | static: help alerts: "Interactive runs dispatch immediately"; not observed (no --notify allowed). |
| notify-32 | 81-82 | Interactive runs don't open an incident | default | shell: run notify-30, then check Dashboard/incidents list shows none | ⚠️ | static: help alerts: "they don't open an incident"; not observed. |
| notify-33 | 82 | Incidents are a cron concept | xref | `help alerts` / crons doc mentions incidents | ✅ | static: help alerts: "(incidents are a cron concept)" and "in a cron, that stderr is what drives incidents and notifications" |

## Style observations
- Line 28-29 refers to `help alerts` while crons.md refers to `man alert` and **Alerting strategies**; check help topic naming is consistent.
- Line 14 "four service types" vs headings "Connecting Slack and PagerDuty"; fine.
- Line 41 sample key `R0UTING_KEY_FROM_PD` has a zero in "R0UTING" (likely intentional placeholder, looks like typo).
- Heading levels: two `##` sections after the page `## Notifications`; no `###` used (page title is also `##`), so hierarchy is flat.
- No warp, Windows, "watch", or "coming soon" mentions.

## Summary
doc: docs/manual/notify.md
claims: 33
by category: black-box runnable ~11; needs-human ~18; unverifiable ~4 (approximate)


## Style observations: results
- `help alerts` topic name exists and is consistent with the doc; crons.md uses `man alert` (separate page, not checked here).
- `R0UTING_KEY_FROM_PD` zero is a placeholder typo-looking string only; harmless but reads as a typo.
- Generated notify.ts: `&lt;alias&gt;` is correct escaping inside `<code>`; no stray `{{}}` placeholders (static).

## Findings
- notify-13 (line 34): doc says `weft integration` subcommand exists; true, but `weft integration --help` errors ("unknown integration command: --help"). Product nit; doc fine.
- notify-12 (lines 33-34): "credentials behind them live with your account" is true for Slack/PagerDuty, but webhook headers (e.g. X-Signing-Secret) are stored in plaintext in notify.yaml (live file shows one). Doc ambiguous; suggested addition: "Webhook headers, including any secrets, are written in notify.yaml itself."
- notify-26 (line 59): heading "need no setup" but email needs SMTP (line 73). Ambiguous; suggest heading "Webhook and email need no account connection".
- notify-30 (lines 77-79): example `alert --notify=ops` fires real destinations; consider noting that it sends to every target in `ops`. Not executed here.
- notify-14/15, 06/10/28: need browser or /etc write; see evidence cells.
