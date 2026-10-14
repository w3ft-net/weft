---
id: manual-schema-setup
title: Check schema setup
web:
  section: schema-setup
  category: connect-source
  order: 45
---

## Check schema setup

Once weftd is shipping logs, weft auto-detects the schema from the
first batch of lines. Go to **Namespaces** to see detected apps and
their field schemas.

If you see a **pending source** notification on the dashboard, weft
has received data and needs you to confirm the detected
schema. Review the sample lines and approve.

Two fields come from parsing each log line per the source's schema:

- `TS` — timestamp
- `LEVEL` — log level (ERROR, WARN, INFO, DEBUG)

Every other dimension (`SERVICE`, `HOST`, `ENV`, `REGION`, ...) is stamped by
weftd, the agent token, or cloud metadata rather than parsed from the line;
[Meta-variables](metavars.md) has the full table and how each is sourced.
