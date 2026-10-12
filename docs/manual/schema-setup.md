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

Other fields (`SERVICE`, `HOST`, `DEPLOYMENT`, `ENV`, `VERSION`,
`REGION` (empty when not configured), `CLUSTER`, `K8S_NAMESPACE`, `POD`,
`CONTAINER` (k8s mode), and `NODE`) are stamped by weftd, the agent
token, or cloud metadata — see
**Meta-variables** (`man metavars`) for the full table and how each is
sourced.
