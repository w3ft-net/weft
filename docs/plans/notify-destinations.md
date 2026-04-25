# Plan: Additional notify destinations

Extend `/etc/notify.conf` beyond Slack + PagerDuty to cover generic webhook, email, Opsgenie, and Microsoft Teams. Minimum change to cover the majority of customers who don't run PagerDuty.

## Why add this

`coordinator/notify.go` today dispatches to exactly two services. Every customer who uses Opsgenie / Teams / a homegrown router / "just email me" currently has no path. Generic webhook alone covers ~80% of that demand (users plug it into Zapier, their own router, Discord, Mattermost, whatever); email covers the small-team "we don't have a pager service" case with the SMTP config `api/email.go` already uses for magic links.

## Architecture

`/etc/notify.conf` already has a list-per-service structure:

```yaml
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
```

Four new top-level keys alongside `slack:` and `pagerduty:`:

```yaml
ops:
  slack: ["#alerts"]
  pagerduty: ["ops-daytime"]
  webhook:
    - "https://hooks.example.com/weft/ops"
    - url: "https://intake.example.com/alerts"
      headers:
        X-Signing-Secret: "s3cret"
  email:
    - "oncall@example.com"
    - "paul@example.com"
  opsgenie:
    - ops-api-team
  teams:
    - "#weft-alerts"
```

Each entry is either a bare string or a mapping; the Notifier fans out exactly the same way it does today.

## Components to build

1. **`store.NotifyAlias`** — extend with `Webhook`, `Email`, `Opsgenie`, `Teams` lists; update `ParseNotifyAliases` to accept both string and mapping forms (matching the existing Slack/PD grammar).
2. **`coordinator/notify.go`**:
   - `sendWebhook(target, watchName, body)` — POST typed JSON `{ watch, alias, ts_unix, severity, body }`; 10s timeout; surface non-2xx back to the caller the same way Slack errors surface today.
   - `sendEmail(target, watchName, body)` — call into `api/email.go`'s SMTP config. Requires the SMTP config to move into a shared package (`internal/email/`) so both `api` and `coordinator` can import it without an import cycle. One-line body goes in a plain-text email with the watch name as subject.
   - `sendOpsgenie(target, watchName, body)` — POST to `api.opsgenie.com/v2/alerts` with the integration API key stored under Account → Integrations. Alias dedup using `watchName`, mirroring the PagerDuty dedup key.
   - `sendTeams(target, watchName, body)` — incoming-webhook URL; JSON payload per Adaptive Card schema.
3. **Integration credentials store** — `Opsgenie` and `Teams` need an API key / webhook URL bound to the customer (same pattern as `SlackConfig` in `store/shell.go`). Add `OpsgenieConfig`, `TeamsConfig`.
4. **Frontend — Account → Integrations**:
   - New tabs for Webhook (no credentials needed; URLs are per-alias), Email (SMTP is managed; just a per-alias list), Opsgenie (API key + default team), Teams (webhook URL).
5. **Docs**:
   - Extend `docs/shell/help-alerts.md` and `docs/shell/man-alert.md` with the new keys.
   - Short grammar example for each service in `man alert`.
6. **Tests** — extend `coordinator/notify_test.go` (if present) or add one; httptest server per destination; assert request shape.

## Webhook payload format

Kept intentionally simple, versioned via a top-level `v` field:

```json
{
  "v": 1,
  "watch": "error-rate-hourly",
  "alias": "ops",
  "ts_unix": 1713983420,
  "condition": "above 50",
  "value": "127",
  "severity": "error",
  "body": "error-rate-hourly: 127 (above 50)"
}
```

Adding fields later is additive; renaming bumps `v`.

## Estimated effort

- Webhook (most demanded, simplest): **0.5 day**
- Email — extract shared SMTP helper + wire send: **0.75 day**
- Opsgenie — API key CRUD + dispatch: **0.5 day**
- Teams — webhook URL CRUD + Adaptive Card payload: **0.5 day**
- Config UI tabs: **1 day**
- Docs + tests: **0.5 day**

**Total: ~3 days** for all four; webhook + email alone is ~1.5 days and covers the bulk of demand.

## Non-goals

- **No SMS.** Twilio adds a paid-dependency surface and a delivery-reliability problem we don't want to own.
- **No Discord / Mattermost / Slack-compatible variants.** All of them accept a generic incoming-webhook URL — use `webhook:`.
- **No retry queue.** Best-effort, 10s timeout per target, log the failure. Same behavior as today's Slack/PD.
- **No per-recipient silencing.** That belongs in `docs/plans/alert-silencing.md`.
