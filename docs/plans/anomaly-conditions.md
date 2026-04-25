# Plan: Anomaly alert conditions

Two new alert conditions — `outlier` and `seasonal` — that compare the current run to the watch's own history. Uses existing watch-result storage; no new infrastructure, no ML model.

## Why add this

Today's alert conditions (`above`, `below`, `eq`, `changed`, `rising N%`, `falling N%`) cover fixed-threshold and simple trend cases. They don't cover:

- **"Error rate is higher than usual for a Tuesday 2pm."** Holiday weekends and overnights have different baselines; a fixed `above 50` either pages during normal peaks or misses real spikes in quiet windows.
- **"This host's memory is way out of line with its peers."** Threshold alerts per-host require knowing the right threshold for each host; an outlier alert just says "this one is drifting."

Every mature monitoring tool ships some flavor of anomaly condition. We can ship a credible one without any ML by leaning on the stddev/avg helpers we already have and the watch history we already persist.

## New conditions

### `outlier N`

"Fire if the current value is more than N standard deviations from the rolling average over `--window`."

```
<pipeline> | alert outlier 3 --window 1h --notify=ops
```

Implementation: re-uses `weft.avg(name, window)` and `weft.stddev(name, window)`. Fires when `|current - avg| > N * stddev`. If stddev is zero or the window has < `--min-samples` runs (default 20), falls through quietly — no false positive on cold start.

Direction is symmetric by default; `--above` / `--below` restrict to one-sided.

### `seasonal N --period <dur>`

"Fire if the current value is more than N stddev from the mean of same-time-of-period values over `--window`."

```
<pipeline> | alert seasonal 3 --period 1d --window 4w --notify=ops
```

Example: `period: 1d, window: 4w` → compares right-now to the 4 values captured at this time on the last 4 days. `period: 1w, window: 8w` compares to the same hour of the week over 8 weeks.

Implementation: a new store helper `store.ScheduledCronResults(name, period, window)` that returns the set of runs whose `bucket_start % period == now % period` within the window. Mean + stddev over that set; same fire rule as `outlier`.

Seasonal needs more history to be useful. Gated on `min_samples` (default 4) per bucket; below that, no fire.

## Why not forecast-style conditions

We considered an `alert forecast` that linearly extrapolates and fires when the current value deviates from the projected line. Three problems:

1. Needs a model choice (linear, exponential smoothing, ARIMA). Every choice is wrong on some shape of data; defending the default is a forever-cost.
2. Warm-up behavior is subtle. Either the alert is quiet for a long time while the model converges, or it fires spuriously early.
3. The workaround is pretty clean: wrap `weft.avg(x, window)` as the watch body and compare with `alert rising N%`. You get a trend alert without us having to own a forecasting library.

Not shipping `forecast` in v1. `outlier` and `seasonal` cover the real use cases cleanly.

## Components to build

1. **`shellcore/alert_args.go`** — parser additions for `outlier N`, `seasonal N`. Same `--notify`, `--msg`, `--duration` treatment as existing conditions.
2. **`coordinator/alert.go`** — condition evaluator. Delegates math to new helpers in `store/user.go`:
   - `OutlierStats(customerID, watchName, window)` → `{avg, stddev, samples}`.
   - `SeasonalStats(customerID, watchName, period, window)` → `{avg, stddev, samples}` over the bucket-aligned subset.
3. **`store/user.go`** — two queries against `cron_results` (or the rollup tables when it makes sense). Both are pure SQL.
4. **Docs** — extend `docs/shell/man-alert.md` and `docs/shell/help-alerts.md` with the new conditions plus a section on "when to use outlier vs. seasonal vs. rising/falling."
5. **Tests** — `coordinator/alert_outlier_test.go` + `coordinator/alert_seasonal_test.go` with synthetic cron history (stable, drifting, seasonal).

## Gotchas surfaced in review

- **Watches with string output.** Existing watch schema allows non-numeric stdout (`weft.latest` returns a string). Outlier/seasonal require numeric parseability; runs that don't parse are excluded from the stats calculation (same behavior as today's aggregates).
- **Zero stddev.** `outlier` with a perfectly flat history would fire on any departure at all. We cap: if `stddev < 0.01 * |avg|` (floor 1e-9), treat as "no signal" and don't fire. Customer can fall back to `changed` for that case.
- **Sliding vs. stepped windows.** Windows are sliding, ending at `now`. Not stepped to clock-aligned buckets. Consistent with how `weft.avg` already works.
- **Alert-time tz choice for `seasonal`.** Period alignment uses UTC. Customer watches typically care about local-business-hours effects; noted in docs ("if you want 'same hour America/New_York' semantics, schedule your watch at a higher frequency in the business-hour window"). A real tz-aware period is a follow-up if customers name it.

## Estimated effort

- Grammar parse in `shellcore/alert_args.go`: **0.5 day**
- Condition evaluator + store queries: **1 day**
- Docs (man page + help page + "when to use" section): **0.5 day**
- Tests with synthetic series: **0.75 day**

**Total: ~2.75 days**.

## Non-goals

- **No ML model.** Stddev-from-mean is the entire algorithm, both for outlier and seasonal. When a customer names a case this misses, we revisit.
- **No `alert forecast`.** See "Why not forecast-style conditions" above.
- **No custom baseline windows per run.** The `--window` is a scalar duration, not an arbitrary query. The baseline is always the watch's own history.
- **No cross-watch anomaly.** "Fire when watch A is out of line with watch B" is composite alerting, see `docs/plans/composite-alerts.md`.
- **No automatic baseline tuning.** Customer picks `N` (typical: 3). Auto-tuning is the kind of feature that looks magical in demos and silently regresses in production.
