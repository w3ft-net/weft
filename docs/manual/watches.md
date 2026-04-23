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

Every watch run is stored. You can pull summaries from other watches
or one-off pipelines:

- `weft.latest <watch>` — most recent value
- `weft.avg <watch> <duration>` — rolling average over window
- `weft.max <watch> <duration>` — window maximum
- `weft.min <watch> <duration>` — window minimum
- `weft.sum <watch> <duration>` — window sum
- `weft.stddev <watch> <duration>` — window standard deviation

```
# Chain: average of one watch triggers another
echo $(weft.avg error-rate 1h) | alert above 50 --notify=ops
```

Tier limits (number of watches and minimum schedule interval) vary by
plan — see the **Account** page for your current caps, or run
`help cron` in the shell.
