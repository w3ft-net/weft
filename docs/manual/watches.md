---
id: manual-watches
title: Watches
web:
  section: watches
  order: 50
---

## Set up a watch

Watches are saved pipelines that run on a schedule. Create one from
the shell:

```
# Open the watch editor
vi /cron.d/error-count

# Watch file format: a '# schedule:' directive plus a pipeline.
# schedule: */5 * * * *
cat myapp.log --since 5m | awk '$LEVEL == "ERROR"' | wc -l | alert above 0 --notify=ops
```

The pipeline's stdout is captured as the watch result — numeric
values are graphed on the dashboard as sparklines. Alerts are
dispatched by the `alert` stage (see the Alerting section); there is
no separate watch-level notify field.

View and manage watches from the **Dashboard**.

### Accessing past results

Every watch run is stored. The `weft.*` helpers pull summaries out of
that history — use them from the shell prompt, from a `/bin/` script,
or from another watch's pipeline:

| Helper | Returns | Notes |
|---|---|---|
| `weft.latest <watch>` | string | Trimmed stdout of the most recent run |
| `weft.avg <watch> <duration>` | number | Mean of numeric results in the window |
| `weft.max <watch> <duration>` | number | Largest numeric result in the window |
| `weft.min <watch> <duration>` | number | Smallest numeric result in the window |
| `weft.sum <watch> <duration>` | number | Sum of numeric results in the window |
| `weft.stddev <watch> <duration>` | number | Population stddev; requires ≥ 2 results |

**Durations** are the usual Go syntax — `30s`, `5m`, `1h`, `6h`, `24h`
— and must be positive and ≤ 24 hours. Longer windows are rejected.

**Numeric vs string.** `weft.latest` just returns whatever the watch's
last run wrote to stdout, so a watch that prints `ERROR: disk full`
and a watch that prints `42` both come back as strings. The
aggregates, by contrast, parse each run's stdout as a float and
silently skip any rows that don't parse — they return an error if
nothing in the window is numeric (and `weft.stddev` also errors when
the window has fewer than 2 numeric samples).

**Retention.** The aggregates read back through whatever history your
plan's retention covers; older runs are invisible to the helpers even
if they're still on disk.

The helpers are available everywhere the shell is:

```
# Interactively, at the prompt
weft.latest error-rate

# Chained through a pipeline
echo $(weft.avg error-rate 1h) | alert above 50 --notify=ops

# From another watch — aggregate a noisy-per-minute watch into an hourly alert
# /cron.d/error-rate-hourly
# schedule: every 5m
echo $(weft.avg error-rate 1h) | alert above 50 --notify=ops

# From a Lua /bin/ script — return type is a number, no string parsing
#   local rate = weft.avg("error-rate", "1h")
```

Tier limits (number of watches and minimum schedule interval) vary by
plan — see the **Account** page for your current caps, or run
`help cron` in the shell.
