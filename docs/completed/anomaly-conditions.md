# Plan: Anomaly alert conditions [DONE]

> **Status: Shipped — different surface than originally designed.**
> This plan proposed two new `alert` *conditions* (`alert outlier N`,
> `alert seasonal N`) parsed in `shellcore/alert_args.go`. What
> actually shipped is the same math and semantics exposed as two
> members of the **predicate family** instead: `weft::is_outlier` and
> `weft::is_seasonal` (each with a Lua mirror `weft.is_outlier(...)`).
> This matches how the rest of the condition vocabulary settled
> (`weft::is_above`, `weft::is_rising`, …) — predicates compose with
> `&&`/`||` and feed a single `alert --if-zero "$?"`, rather than each
> being bespoke `alert` grammar. The original design rationale,
> guards, and non-goals below all hold; only the invocation surface
> changed. See "What shipped" for the real entry points.

Two anomaly checks — outlier and seasonal — that compare the current
run to the watch's own history. Uses existing watch-result storage; no
new infrastructure, no ML model.

## Why add this

Today's alert conditions (`above`, `below`, `eq`, `changed`, `rising N%`, `falling N%`) cover fixed-threshold and simple trend cases. They don't cover:

- **"Error rate is higher than usual for a Tuesday 2pm."** Holiday weekends and overnights have different baselines; a fixed `above 50` either pages during normal peaks or misses real spikes in quiet windows.
- **"This host's memory is way out of line with its peers."** Threshold alerts per-host require knowing the right threshold for each host; an outlier alert just says "this one is drifting."

Every mature monitoring tool ships some flavor of anomaly condition. We can ship a credible one without any ML by leaning on the stddev/avg helpers we already have and the watch history we already persist.

## The checks (as shipped)

### `weft::is_outlier <watch> <n_stddev> <window> [--above|--below]`

"Exit 0 if the current value is more than `n_stddev` standard
deviations from the rolling average over `window`."

```
weft::is_outlier error-rate 3 1h --above
alert --if-zero "$?" --notify=ops
```

Re-uses `WeftFuncs.Avg(name, window)` and `WeftFuncs.Stddev(name,
window)`. Fires when `|current - avg| > n_stddev * stddev`. Direction
is symmetric by default; `--above` / `--below` restrict to one side.
Cold-start / no-signal cases fall through quietly (see Guards).

### `weft::is_seasonal <watch> <n_stddev> <period> <window>`

"Exit 0 if the current value is more than `n_stddev` stddev from the
mean of same-time-of-period values over `window`."

```
weft::is_seasonal error-rate 3 1d 4w
alert --if-zero "$?" --notify=ops
```

Example: `period=1d window=4w` → compares right-now to the values
captured at this time on the last ~4 days. `period=1w window=8w`
compares to the same hour of the week over 8 weeks. `window` must be
strictly larger than `period` (enforced; exit 2 otherwise). Backed by
`store.SeasonalCronStats`, which returns avg/stddev/samples over the
`bucket_start % period == now % period` subset within the window.

## Why not forecast-style conditions

We considered a `forecast` check that linearly extrapolates and fires when the current value deviates from the projected line. Three problems:

1. Needs a model choice (linear, exponential smoothing, ARIMA). Every choice is wrong on some shape of data; defending the default is a forever-cost.
2. Warm-up behavior is subtle. Either the alert is quiet for a long time while the model converges, or it fires spuriously early.
3. The workaround is pretty clean: wrap `weft.avg(x, window)` as the watch body and compare with `weft::is_rising`. You get a trend alert without us having to own a forecasting library.

Not shipping `forecast`. `outlier` and `seasonal` cover the real use cases cleanly.

## What shipped

1. **`coordinator/weftpredicates.go`** — `predicateOutlier` (line
   303) and `predicateSeasonal` (line 359), registered as
   `weft::is_outlier` / `weft::is_seasonal` via
   `RegisterCommandMeta` (lines 55–63) with `TypeDoc` usage strings.
   (Replaces the proposed `shellcore/alert_args.go` grammar; no
   `alert outlier` keyword was added.)
2. **`coordinator/weftfuncs.go`** — `WeftFuncs.Avg`, `Stddev`, and
   `SeasonalStats` (line 273) returning a `SeasonalStatsResult`
   (line 36). These are the math helpers; the Lua mirrors hang off
   the same `WeftFuncs`.
3. **`store/shell.go`** — `SeasonalCronStats(customerID, watchName,
   period, window)` (line 814): the bucket-aligned avg/stddev/samples
   SQL over `cron_results`. (The plan proposed putting this in
   `store/user.go`; it landed in `store/shell.go` alongside the other
   cron-result queries.)
4. **Docs** — `docs/shell/help-alerts.md` lists both predicates in
   the predicate family (lines 45–51); the prose guidance and the
   per-function reference live in `docs/manual/alerting-strategies.md`
   and `docs/manual/weft-funcs.md` (not the `docs/shell/man-alert.md`
   the plan originally named).
5. **Tests** — `coordinator/weftpredicates_test.go`:
   `TestPredicate_Outlier` (line 81) and
   `TestPredicate_Outlier_FlatHistoryNoFire` (line 106), plus Lua
   coverage in `coordinator/weftfuncs_lua_test.go`.

## Guards (implemented)

- **Watches with string output.** Non-numeric stdout is excluded
  from the stats calculation; `latestFloat` returning not-ok yields
  exit 1 (no fire), same as today's aggregates.
- **Zero / near-zero stddev.** A perfectly flat history would fire on
  any departure. Both predicates floor stddev at `0.01 * |avg|` (hard
  minimum `1e-9`); below the floor there's no signal and they return
  1. (`weftpredicates.go:340-346`, `:394-400`.)
- **Cold start.** `is_seasonal` requires ≥ 4 same-time samples
  (`stats.Samples < 4` → no fire, `:391`); `is_outlier` relies on the
  stddev floor plus `Avg`/`Stddev` erroring out on thin history.
- **Sliding vs. stepped windows.** Windows are sliding, ending at
  `now`. Not stepped to clock-aligned buckets. Consistent with how
  `weft.avg` already works.
- **Alert-time tz for `seasonal`.** Period alignment uses UTC. A
  real tz-aware period is a follow-up if customers name it.

## Non-goals (still in force)

- **No ML model.** Stddev-from-mean is the entire algorithm for both
  outlier and seasonal.
- **No `forecast` check.** See "Why not forecast-style conditions".
- **No custom baseline windows per run.** `window` is a scalar
  duration; the baseline is always the watch's own history.
- **No cross-watch anomaly.** "Fire when watch A is out of line with
  watch B" is composite alerting, see
  `docs/completed/composite-alerts.md`.
- **No automatic baseline tuning.** Customer picks `n_stddev`
  (typical: 3).
