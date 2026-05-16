---
id: manual-crons
title: Crons
web:
  section: crons
  category: alert-visualize
  order: 50
---

## Set up a cron

Crons are saved pipelines that run on a schedule. Create one from
the shell:

```
# Open the cron editor
vi /cron.d/error-count

# Cron file format: a '# schedule:' directive plus a pipeline.
# schedule: */5 * * * *
tail -n 500 myapp.log | awk '$LEVEL == "ERROR"' | wc -l | alert above 0 --notify=ops
```

The pipeline's stdout is captured as the cron result — numeric
values are graphed on the dashboard as sparklines. Alerts are
dispatched by the `alert` stage (see **Alerting strategies** for
the catalogue of alert shapes, plus `man alert` for the builtin's
grammar); there is no separate cron-level notify field.

View and manage crons from the **Dashboard**.

### Accessing past results

Every cron run is stored. The `weft::*` family pulls summaries out
of that history (aggregates like `weft::avg`, point reads like
`weft::latest`) and a parallel predicate family (`weft::is_above`
etc.) returns booleans for use in composite alerts. They're
available at the shell prompt, inside `/bin/` scripts, and as
pipeline stages in other crons:

```
# Interactively, at the prompt
weft::latest error-rate

# Chained through a pipeline
echo $(weft::avg error-rate 1h) | alert above 50 --notify=ops

# From a Lua /bin/ script — return type is a number, no string parsing
#   local rate = weft.avg("error-rate", "1h")
```

The full reference (value funcs, predicates, stdin comparators,
caps, numeric-parsing rules) is in **Weft functions**. Patterns
that combine these into alerts of various shapes — multi-row
per-tag, composite, anomaly-based — live in **Alerting strategies**.

Tier limits (number of crons and minimum schedule interval) vary by
plan — see the **Account** page for your current caps, or run
`help cron` in the shell.

### Cron version history

Every save to `/cron.d/<name>` is captured. After `:w`, the shell
prompts for an optional commit message; `wit log /cron.d/<name>`
lists past versions, `wit checkout /cron.d/<name> HEAD^2` rolls
back. Five most recent past versions are kept. See **wit** for
the full surface.
