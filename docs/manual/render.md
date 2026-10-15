---
id: manual-render
title: Connect a Render service
web:
  section: connect-render
  category: connect-source
  order: 37
---

## Connect a Render service

Weft accepts logs directly from Render's built-in **Log Streams** feature using
syslog over TLS. This page covers the Render-specific parts; the steps every
source shares are in [Connect a log source](connect-source.md).

### 1. Create a token

Create a namespace token as described in
[Connect a log source](namespaces.md#creating-a-token). Render tokens are
ordinary namespace tokens, the same kind you use for any other source.

### 2. Add the log stream in Render

In the Render dashboard, click **Integrations → Observability** in the sidebar.
Scroll down to **Log Streams**, add a stream, and configure a generic/custom
syslog destination. Paste in the token from Step 1, along with the endpoint
(`render.syslog.w3ft.net:6514`).

Render starts streaming within a few seconds. If you want parsed fields,
approve a schema ([step 4](connect-source.md#sampling-logs-for-a-schema) of
the shared steps). The schema is per Render service, so it covers the web,
cron and build files under it.

## Reading your logs

To confirm rows are landing, see
[Connect a log source](connect-source.md#sampling-logs-for-a-schema). Weft maps Render services to log files as follows:

| Render source                       | weft file                                  | metavariables |
|---|---|---|
| `myservice` web instance `web-9nk6q` | `/log/<ns>/myservice/web.log`  | instance=9nk6q |
| `myservice` cron job `cron-stqq5`    | `/log/<ns>/myservice/cron.log`                   | instance=stqq5 |
| `myservice` build output `bld-4fk2b`   | `/log/<ns>/myservice/bld.log`                    | instance=4fk2b |

If you specify an env label for your token has an env bound, the path will
include that segment, e.g. `/log/<ns>/prod/myservice/web.log`. See
**Namespaces** for the layout rules.
