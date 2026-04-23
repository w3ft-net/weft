---
id: manual-schema-setup
title: Check schema setup
web:
  section: schema-setup
  order: 45
---

## Check schema setup

Once weftd is shipping logs, weft auto-detects the schema from the
first batch of lines. Go to **Namespaces** to see detected apps and
their field schemas.

If you see a **pending source** notification on the dashboard, it
means weft received data but needs you to confirm the detected
schema. Review the sample lines and approve.

Common fields weft extracts automatically:

- `$TS` — timestamp
- `$LEVEL` — log level (ERROR, WARN, INFO, DEBUG)
- `$APP` — application name
- `$INSTANCE` — hostname or instance ID
