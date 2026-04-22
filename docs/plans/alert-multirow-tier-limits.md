# Plan-tier limits for multi-row alerts (future work)

Placeholder for the tier policy around the multi-row alert feature
(`alert -k N -t N` over a pipeline that emits many lines per tick).
Nothing is enforced yet beyond the existing `WatchLimit` and
`MinWatchInterval` knobs in `store/user.go`; this doc captures what
we'll want to add once the feature has real usage and we can see
the actual blast radius.

## Why this matters

In single-row mode, one watch tick = one `cron_results` row. A watch
running every minute produces 1,440 rows/day.

In multi-row mode, one tick = N rows, one per tag. The multiplier is
under the user's control: their pipeline decides how many tags come
out of each tick. A disk-usage watch that emits one row per shard is
bounded by the fleet size; a watch that groups by `(host, endpoint,
status)` can emit thousands.

Worst case today: a free-tier (`swatch`) customer with a 5-minute
schedule floor, a pipeline producing 1,000 lines per tick, and a 30-day
retention window = ~8.6M `cron_results` rows for one watch. That's
within what pg_partman handles, but it's asymmetric — cheap for the
customer to create, expensive for us to store and query — and it
stresses the per-tag aggregation indexes that `alert --duration` and
`rising` / `falling` rely on.

## Knobs to add

Roughly in order of how much we'd miss each one:

1. **`MaxRowsPerTick(tier)`** — clamp the number of cron_results rows
   written per tick per watch. Excess lines get truncated, a single
   warning line lands in `stderr`, and the watch keeps running.
   Save-time check is impossible (pipeline output is dynamic); runtime
   enforcement only. This is the primary lever: once we cap rows per
   tick, rows per day fall out of the existing `MinWatchInterval`.

2. **`MaxTagCardinality(tier)`** — soft ceiling on *distinct tag
   values observed over the retention window*, checked by a periodic
   job. If a watch blows past this, we open a visible warning on the
   watch's status and stop opening new per-tag incidents (existing
   tags keep working). Different from #1 because a watch could stay
   under `MaxRowsPerTick` while still accumulating distinct tags over
   time (e.g., ephemeral container IDs as tags).

3. **`MaxCronResultsBytesPerCustomer(tier)`** — storage ceiling across
   all of a customer's watches. Checked by the same periodic job as
   #2. Triggers the same kind of visible warning plus a hard stop on
   new watch creation until the customer either upgrades or deletes
   something. Pairs with the existing `WatchLimit` (count) knob but
   governs the storage dimension that `WatchLimit` alone doesn't.

## Suggested starting values

Nothing here is load-tested; treat as a first pass to be revisited
once we have real customer data. Units chosen to be legible, not
operationally tight.

| Tier   | MaxRowsPerTick | MaxTagCardinality | MaxCronResultsBytes |
|--------|----------------|-------------------|---------------------|
| trial  | —              | —                 | — (watches off)     |
| swatch | 50             | 500               | 100 MB              |
| yard   | 500            | 5,000             | 1 GB                |
| bolt   | 5,000          | 50,000            | 10 GB               |

`MaxRowsPerTick` deliberately leaves generous headroom over anything
a normal ops watch would produce. `swatch` at 50 rows covers "one row
per shard in a small fleet" or "one row per endpoint in a small
app"; `bolt` at 5,000 covers "one row per container in a production
fleet."

`MaxTagCardinality` is the number to watch most carefully — the mode
of failure we care about is unbounded-cardinality tags (UUIDs,
request IDs), where a legitimate-looking watch quietly consumes
storage for months.

## Enforcement shape

- **`MaxRowsPerTick`** lives in the cron executor, next to where
  `SaveCronResults` gets called. Truncate + emit one warning line
  in `stderr` with the row count and the limit that hit. The
  truncated rows do *not* participate in alert evaluation.
- **`MaxTagCardinality`** and **`MaxCronResultsBytesPerCustomer`**
  run from a pg_cron job (daily, same schedule shape as
  `usage-rollup` in migration 002) against `cron_results` and write
  to a new `watch_limit_breaches` table. The coordinator reads that
  table when listing watches and surfaces a warning in `vi
  /cron.d/foo` and in the watch-list API.
- No retroactive enforcement on existing rows. Partitions age out
  on their own.

## Observability

Three things the user needs to see when limits bite, in order of
where they'll look:

1. The watch status pane / `ls /cron.d/` output flags a limit breach.
2. `stderr` of an affected tick carries the truncation / skip line.
3. The customer's billing/account page shows current usage vs. the
   tier ceiling so they can decide between pruning, upgrading, or
   both.

None of that UI exists today — the watch-list API returns only the
`LastStatus` string. Adding a `LimitWarnings []string` field to the
watch response object, and rendering it in the shell, is the minimum
to make this feature usable.

## What this doc is not

- It doesn't design the Stripe metered-billing side. That's covered
  (or not covered) by `stripe.md`. If multi-row alerts become a
  major storage driver we'll want to push `cron_results_bytes` into
  the billing metric set, but that's downstream of this.
- It doesn't cover migration from today's implicit-unlimited state
  to enforced limits. When we turn this on, a grandfather clause
  for watches that already exceed limits is probably kind; working
  that out is part of the rollout, not the design.

## Trigger for picking this up

Open this plan again when any of the following happens:

- A single customer's `cron_results` grows past ~10 GB.
- A single watch produces >500 tags in any 24h window.
- A support ticket mentions "my watches are slow" and the cause is
  a large `cron_results` scan.
- We ship metered billing (Stripe plan above) and need a usage
  metric for the alert surface.

Until one of those lands, `WatchLimit` + `MinWatchInterval` are
enough.
