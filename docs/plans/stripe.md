# Overage billing through Stripe (future work)

The pricing page on the landing (`frontend/src/components/PricingTable.tsx`)
advertises monthly ingestion quotas and per-GB overage rates:

| Tier   | Quota        | Overage    |
|--------|--------------|------------|
| Swatch | 10 GB/month  | $2/GB      |
| Yard   | 100 GB/month | $1/GB      |
| Bolt   | 1 TB/month   | $0.50/GB   |

**Neither the quota nor the overage is billed today.** A customer on Swatch
who ingests 50 GB in a month pays $19 for the subscription and $0 for the
40 GB they pulled in over quota. The usage-progress bar on the dashboard
happily renders `500% · 50.0 GB / 10.0 GB`; nothing else happens.

This is the billing analog of the pre-fix log-retention mismatch: marketing
promises a commercial outcome that the code doesn't deliver. This plan
captures what it takes to deliver it.

## Why this matters

- **Revenue capture.** Every GB over quota on a swatch/yard customer is
  advertised at a real dollar amount we're not charging. Internal usage
  data (below) makes it trivial to compute the forgone revenue.
- **Commitment alignment.** Customers understand the $-per-GB page as a
  promise. Shipping the quota-only billing preserves the gap forever.
- **Fair play between tiers.** A swatch customer can pull 100× their
  quota without overage risk, so there's no financial incentive to
  upgrade to yard. Tiering isn't real without the overage teeth.

## Current state

### Usage data (in place and accurate)

`store/usage.go` exposes two calls:

- `UpsertUsageHourly(customer, hour, bytes)` — called by the ingest path
  on every flushed batch. Writes to `usage_hourly`.
- `QueryUsage(customer, periodStart, periodEnd)` — UNION ALLs
  `usage_hourly` + `usage_daily` and sums. Feeds the dashboard's usage
  card and `/api/billing/usage`.

Partition layout (migration 002):

- `usage_hourly` — partitioned by day, `pg_partman` retains 7 days.
- `usage_daily` — partitioned by month, retains 13 months.
- A `pg_cron` job at 02:00 UTC rolls hourly→daily and deletes the
  rolled-up hourly rows.

Known concerns that touch billing correctness:

- **`UsageRecord` struct is dead.** Defined in `store/user.go`, no
  callers. Either delete it or repurpose as the audit log for
  Stripe-reporting (see below).

Previously this list included a non-atomic rollup (an INSERT + DELETE
pair running as two separate transactions, double-counting for a
reader in between). Migration 028 switched the rollup to the
idempotent `ON CONFLICT DO UPDATE` pattern from migration 026 —
usage_hourly rows are no longer deleted by the rollup (pg_partman
handles the 7-day drop), and `QueryUsage` reads usage_daily for
completed days and usage_hourly for today to avoid the new 7-day
overlap.

### Stripe subscription billing (in place)

`api/handlers.go`:

- `StripePrices map[string]string` — `tier → Stripe price ID`. Today
  only the flat-rate tier price.
- `handleSetupIntent` and `handleCheckout` create/update subscriptions
  with a single subscription item pointing at the tier's price.
- `handleStripeWebhook` handles `customer.subscription.updated`,
  `invoice.payment_failed`, `customer.subscription.deleted`. Missing
  every event that's relevant to metered billing.
- `store.Subscription.StripeSubscriptionID` stores the subscription ID
  but not the subscription *item* ID — which matters because metered
  usage reports target an item, not the subscription.

### Tier volume allowance (scattered)

`10 GB / 100 GB / 1 TB` literals live in at least three places:

- `api/handlers.go:972-985` — `handleUsage` sets `tier_limit_bytes`.
- `shellfs/shellfs.go:546-554` — duplicate switch for the shell's
  `weft.usage()` accessor.
- `frontend/src/components/PricingTable.tsx:9/18/27` — marketing.

These drifted before we centralized `LogRetention`, and the same thing
will happen for overage rates if we add them without consolidating.

## What to build

### Single source of truth in `store/user.go`

Add two helpers keyed on `Tier`, mirroring the existing pattern:

```go
// Monthly ingestion allowance, matching the pricing page.
func VolumeAllowance(tier Tier) int64 {
    switch tier {
    case TierSwatch: return 10 * 1024 * 1024 * 1024
    case TierYard:   return 100 * 1024 * 1024 * 1024
    case TierBolt:   return 1024 * 1024 * 1024 * 1024
    default:         return 1 * 1024 * 1024 * 1024 // trial
    }
}

// Dollars per GB of overage. Pricing page promise; Stripe is the
// authoritative billing system — this helper is for internal math
// (projections, dashboard UI) only.
func OverageRatePerGB(tier Tier) float64 {
    switch tier {
    case TierSwatch: return 2.00
    case TierYard:   return 1.00
    case TierBolt:   return 0.50
    default:         return 0 // trial: no overage
    }
}
```

Both `handleUsage` and `shellfs.go` switch to `store.VolumeAllowance`.
`PricingTable.tsx` stays hand-authored — it's marketing copy that
sometimes differs deliberately from runtime behavior (introductory
offers, etc.) and shouldn't be generated.

### Stripe model: metered subscription items

Stripe has three billing shapes we could use:

1. **Meters + Meter Events** (Stripe's newest API, shipped 2024). Attach
   a meter to a subscription; report usage via
   `billing.meter_event.create`. Stripe attributes each event to the
   current period based on its timestamp. Best when usage is small and
   continuous; handles backdated events gracefully.
2. **Subscription items with metered pricing** (older but stable).
   Each subscription has two items: one flat-rate (the tier), one
   metered (the overage). Report usage via `usage_records.create`
   against the item's ID. Well-documented, lots of prior art.
3. **Invoice items.** Add arbitrary line items to the next invoice via
   `invoiceitem.create`. Most flexible but least integrated with
   Stripe's usage dashboards.

**Recommendation: option 2** — subscription items with metered pricing.

Reasoning:

- Stripe Meters is newer and has fewer examples in the wild; integrating
  with our `stripe-go/v82` SDK may be more friction than reward.
- Usage records are the tried-and-true path and we already use the SDK.
- The subscription *item* model lets us express the pricing exactly as
  the page advertises: flat-rate subscription + graduated metered
  price with the first N GB free.

Stripe graduated pricing on the metered price can encode "first 10 GB
free, $2/GB thereafter" for a swatch subscription — so we report **raw
ingested bytes**, not pre-subtracted overage, and Stripe does the math.
Simpler bookkeeping on our side.

### Subscription setup changes

`api/handlers.go` `handleSetupIntent` currently creates:

```go
Items: []*SubscriptionItemsParams{{ Price: flatRatePriceID }}
```

Change to:

```go
Items: []*SubscriptionItemsParams{
    { Price: flatRatePriceID },       // e.g. swatch $19/mo
    { Price: overageMeteredPriceID }, // e.g. swatch $2/GB after 10GB
}
```

`StripePrices` becomes:

```go
type TierPrices struct {
    Flat    string // recurring subscription price
    Overage string // metered price for GB-over-quota
}
StripePrices map[Tier]TierPrices
```

And the env:

```
STRIPE_PRICE_SWATCH=price_123
STRIPE_PRICE_SWATCH_OVERAGE=price_456
STRIPE_PRICE_YARD=price_789
STRIPE_PRICE_YARD_OVERAGE=price_abc
...
```

### Track the metered subscription item ID

`store.Subscription` today stores the subscription ID but not the
per-item IDs. Usage records target the item, not the subscription —
so either:

- Query Stripe for the item ID each report (`subscription.Get`,
  pluck the metered item). Simple, but one extra API call per report.
- Store the metered item ID on `store.Subscription`. One DB column
  (`stripe_overage_item_id TEXT`), populated on subscription create and
  refreshed on tier change.

**Recommendation:** store it. A daily reporter firing hundreds of
Stripe API calls per tick is wasteful, and the item ID is stable across
a subscription's lifetime (tier changes create a new item → we update
the column from the webhook).

### Daily reporter cron

A Go routine inside the coordinator (or as a separate `cmd/billing-
reporter` binary, same process either way) that wakes at 03:00 UTC —
one hour after the hourly→daily rollup completes, to ensure
`usage_daily` has yesterday's totals — and does:

1. `SELECT customer_name, SUM(bytes_ingested) FROM usage_daily WHERE
   day = yesterday GROUP BY customer_name`.
2. For each row, look up the active subscription + metered item ID.
3. Compute the Stripe idempotency key:
   `usage:<customer>:<yyyy-mm-dd>`.
4. Call `usagerecord.New(item_id, &params{
       Quantity: bytes, Timestamp: end-of-day, Action: "increment",
       IdempotencyKey: key,
   })`.
5. Record the result in a new `stripe_usage_reports` table (ID,
   customer, day, bytes, stripe_usage_record_id, error, reported_at).

On failure: log and leave the row absent from `stripe_usage_reports`.
The next day's run re-reports it because it wasn't marked done — the
idempotency key protects against double-charging during the replay.

### Tier changes mid-period

Stripe handles flat-rate proration automatically. For the metered
item:

- On tier change, the old metered item is cancelled and a new one
  replaces it. Usage reported against the old item is billed at the
  old rate; usage reported against the new item is billed at the new
  rate. Correct by construction as long as we always report against
  the *current* item.
- The `stripe_overage_item_id` column must be refreshed on
  `customer.subscription.updated` so reports land on the new item.

### Cancellation, refunds, chargebacks

- `customer.subscription.deleted` — stop reporting for that customer.
  Existing reports stand; Stripe raises them on the final invoice.
- Refund / chargeback flows aren't strictly metered-billing concerns;
  existing Stripe-dashboard flows handle them outside of weft.

### Dashboard surfacing

- The usage card on the dashboard should show the expected overage in
  dollars for the current billing period, computed client-side from
  `VolumeAllowance` + `OverageRatePerGB` + cumulative bytes. Same pattern
  as the "tier_limit_bytes" projection today.
- `/api/billing/usage` gains a `projected_overage_cents` field so the
  client doesn't replicate the math.

## Prerequisites / order of operations

1. ~~Fix the rollup atomicity~~ — done in migration 028 (idempotent
   rollup, no DELETE, split-on-today read).
2. **Centralize `VolumeAllowance` + `OverageRatePerGB`** in
   `store/user.go`; update `handleUsage` and `shellfs.go`.
3. **Set up Stripe products + prices** in the Stripe dashboard: one
   flat + one metered with graduated tiers per plan. Put price IDs in
   env config.
4. **Extend subscription wiring** — dual-item `handleSetupIntent`,
   `handleCheckout`; `stripe_overage_item_id` column + migration; a
   webhook branch that refreshes the column on tier change.
5. **Build the reporter** — daily `usage_records.create` with
   idempotency keys, audit log.
6. **Surface projected overage** in the dashboard and
   `/api/billing/usage`.
7. **Delete or repurpose** `UsageRecord` — either drop it, or use it as
   the `stripe_usage_reports` audit table schema (renaming to keep
   callers sane).

Step 2 is a mechanical cleanup that unblocks nothing but is cheap.
Steps 3–7 are the actual metered-billing project and make sense as a
focused follow-up.

## Open questions worth resolving before step 3

- **Free-period policy.** New customers get a trial period; should
  trial users see overage math but not be billed? Stripe handles this
  by leaving trial subscriptions without a metered item, or by setting
  the metered price to zero during trial. Product call.
- **Rounding.** The pricing page says `$2/GB` — do we bill fractionally
  (10.5 GB → $1.00 overage on swatch) or round up each GB (10.5 GB →
  $2.00)? Stripe's graduated pricing supports both via `transform_quantity`
  vs. proportional tiers. Default to fractional (more customer-friendly).
- **Alerting.** A customer about to pay a large overage bill would
  probably rather know in advance. A "you're at 80% of your quota with
  10 days left in the period" email is a useful companion feature —
  out of scope for this doc, but worth flagging so the reporter logs
  enough to feed it.
