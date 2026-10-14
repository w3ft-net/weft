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
syslog over TLS. Here's how to start receiving those logs:

### 1. Creating a token

From the dashboard, go to **Namespaces** and create a namespace (or use an
existing one), then **Create token** within that namespace. The token is
shown only once, so copy it now. Render tokens are ordinary namespace tokens,
the same kind you use for any other source.

Or, from the CLI:

```
weft token create --namespace myco --label render
```

### 2. Add the log stream in Render

In the Render dashboard, click **Integrations → Observability** in the sidebar.
Scroll down to **Log Streams**, add a stream, and configure a generic/custom
syslog destination. Paste in the token from Step 1, along with the endpoint
(`render.syslog.w3ft.net:6514`).

Render starts streaming within a few seconds. 

## Reading your logs

To confirm rows are landing:

```
weft sh -c 'ls /log/<your namespace>'
```

You should see the name of your service as a directory.

Weft maps Render services to log files as follows:

| Render source                       | weft file                                  | metavariables |
|---|---|---|
| `myservice` web instance `web-9nk6q` | `/log/<ns>/myservice/web.log`  | instance=9nk6q |
| `myservice` cron job `cron-stqq5`    | `/log/<ns>/myservice/cron.log`                   | instance=stqq5 |
| `myservice` build output `bld-4fk2b`   | `/log/<ns>/myservice/bld.log`                    | instance=4fk2b |

If you specify an env label for your token has an env bound, the path will
include that segment, e.g. `/log/<ns>/prod/myservice/web.log`. See
**Namespaces** for the layout rules.
