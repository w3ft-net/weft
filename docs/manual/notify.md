---
id: manual-notify
title: Notifications
web:
  section: notify
  category: alert-visualize
  order: 54
---

## Define a notification alias

When an `alert` fires in a cron, `--notify=<alias>` dispatches to a
destination defined in `/etc/notify.yaml`. The alias is a name;
it fans out to one or more targets across four service types — Slack,
PagerDuty, webhook, and email.

```
# /etc/notify.yaml
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
  email:
    - "oncall@example.com"
```

Edit it with `vi /etc/notify.yaml`. See `help alerts` for the full
grammar (severities, webhook headers, per-target mappings).

## Connecting Slack and PagerDuty

`notify.yaml` names destinations — the credentials behind them
live with your account. Connect them from the terminal with `weft
integration` or under **Account → Integrations** in the browser;
both do the same thing.

```
# PagerDuty — paste an integration (routing) key, give it a name.
# The name is what notify.yaml references under `pagerduty:`.
weft integration pagerduty add ops-daytime --key YOUR_ROUTING_KEY
weft integration pagerduty rm  ops-daytime

# Slack — authorize a workspace. Slack uses OAuth, so this opens a
# browser tab to grant access (like `weft login`); the CLI waits and
# confirms once the workspace is connected.
weft integration slack connect
weft integration slack rm

# See what's connected, plus the PagerDuty names notify.yaml can use.
weft integration list
```

A PagerDuty alias in `notify.yaml` must match a name you've registered
with `weft integration pagerduty add`. Slack channels are named
directly in `notify.yaml` (`"#alerts"`) once the workspace is
connected.

## Webhook and email need no account connection

Webhook URLs and email addresses carry no account-level credential.
They are written into `notify.yaml` per alias:

Webhook **header values** are the exception: an `Authorization` token
or a signing secret is a credential, so `cat /etc/notify.yaml` (and
`git log -p /etc/notify.yaml`) shows each one as `"<redacted>"`. To edit,
leave `"<redacted>"` where a value should stay unchanged, or write a real
value to replace it. A `"<redacted>"` on a webhook, header or alias that
has nothing stored is refused.

```
pages:
  webhook:
    - "https://hooks.example.com/weft"
  email:
    - "oncall@example.com"
```

Email delivery requires SMTP to be configured for your deployment.

## Testing an alias

```
echo "test fire" | alert --notify=ops
```

This sends to every target in the `ops` alias.

Interactive runs dispatch immediately and open no incident
(incidents belong to crons).
