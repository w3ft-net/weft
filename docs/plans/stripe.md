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
  `invoice.payment_failed`, `customer.subscription.deleted`. No
  meter-related events are handled, and the meter-events model
  doesn't need any — events are attributed to subscriptions by
  Stripe server-side at ingest, so weft doesn't need to track the
  metered item ID.

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

### Stripe model: meter events + graduated metered price

Stripe's current pattern for "flat-rate + per-unit overage" is:

- A single **meter** records usage events. Attached to one or more
  metered prices via the event name.
- Each paid tier has **two prices** on the same product: a flat-rate
  recurring price, and a **graduated metered price** that references
  the meter. The graduated price's tiers encode "first N units free,
  then $X per unit" directly.
- Subscriptions have **two items**: one per price. Stripe handles
  proration and invoice generation automatically.
- Usage is reported via `POST /v1/billing/meter_events` with the
  customer ID as the only identifier — NOT per-subscription-item. The
  event flows through the meter, hits whichever active metered price
  is currently attached, and lands on the right billing period based
  on the event's timestamp.

Older paths (`usage_records.create` against a subscription item, or
`invoiceitem.create`) are still available but are no longer the
recommended approach for new integrations. We go with meter events.

Practical consequence: **no `stripe_overage_item_id` column is
needed.** Reports use `customer_id`, and tier changes Just Work
because Stripe resolves the active metered price at event-ingest
time.

### Unit of measurement: KB

Reported `value` on each meter event is **kilobytes** (1 KB = 1000
bytes, decimal). The graduated tiers in Stripe are denominated in KB
too. Rationale in the original unit-math discussion (not reproduced
here); short version: KB keeps `unit_amount_decimal` a short decimal,
free-quota boundaries are round numbers, and we don't lose meaningful
resolution on daily reports.

Effect on internal code: `VolumeAllowance` switches from `10 * 1024³`
(GiB) to `10 * 10⁹` (GB decimal) so the "% used" bar in the dashboard
matches what Stripe will bill. This is a 7.4% tightening of the
effective allowance, matching customer expectations of "10 GB."

### Stripe prices (as created)

| Tier   | Flat price ID           | Overage price ID                 | Free quota (KB) | Overage rate     |
|--------|-------------------------|----------------------------------|-----------------|------------------|
| Swatch | `STRIPE_PRICE_SWATCH`   | `price_1TPS462SENTbrh5uDsTsTL8Z` | 10,000,000      | $0.000002 / KB   |
| Yard   | `STRIPE_PRICE_YARD`     | `price_1TPS6V2SENTbrh5uZdLhzDGp` | 100,000,000     | $0.000001 / KB   |
| Bolt   | `STRIPE_PRICE_BOLT`     | `price_1TPSBK2SENTbrh5uSliq28GU` | 1,000,000,000   | $0.0000005 / KB  |

Meter event name: `weft_ingestion_kb`. Aggregation: sum. Configured
on all three overage prices.

### Subscription setup changes

`api/handlers.go` `handleSetupIntent` currently creates:

```go
Items: []*SubscriptionItemsParams{{ Price: flatRatePriceID }}
```

Change to:

```go
Items: []*SubscriptionItemsParams{
    { Price: flatRatePriceID },       // e.g. swatch $19/mo
    { Price: overageMeteredPriceID }, // graduated KB price
}
```

`StripePrices` becomes:

```go
type TierPrices struct {
    Flat    string // recurring subscription price
    Overage string // metered price for KB-over-quota; "" if not configured
}
StripePrices map[Tier]TierPrices
```

Env (flat IDs already present; overage IDs new):

```
STRIPE_PRICE_SWATCH=price_...
STRIPE_PRICE_SWATCH_OVERAGE=price_1TPS462SENTbrh5uDsTsTL8Z

STRIPE_PRICE_YARD=price_...
STRIPE_PRICE_YARD_OVERAGE=price_1TPS6V2SENTbrh5uZdLhzDGp

STRIPE_PRICE_BOLT=price_...
STRIPE_PRICE_BOLT_OVERAGE=price_1TPSBK2SENTbrh5uSliq28GU
```

Absence of the `_OVERAGE` variant → flat-only mode for that tier, so
Stripe config can be piloted one tier at a time.

**Trial tier: no overage item.** Trials get a single-item subscription
matching today's behavior; when a trial converts to a paid tier the
overage item is added as part of the tier-change.

### Daily reporter

A Go routine (coordinator-resident, since that's where the ingest
path already writes to `usage_hourly` / `usage_daily`) fires daily
at 03:00 UTC — one hour after the usage rollup job from migration
028 has settled yesterday's row in `usage_daily`. Each tick:

1. `SELECT customer_name, bytes_ingested FROM usage_daily WHERE
   day = yesterday`.
2. Look up the customer's active subscription. Skip if no paying
   sub (trial / cancelled / deleted).
3. Skip if `stripe_usage_reports` already has a row for
   `(customer, day)` — idempotency at the DB layer.
4. Post to `/v1/billing/meter_events`:
   ```
   event_name           = weft_ingestion_kb
   payload[stripe_customer_id] = <stripe customer id>
   payload[value]       = floor(bytes / 1000)
   identifier           = weft:<customer_name>:<yyyy-mm-dd>
   timestamp            = end-of-day UTC
   ```
   `identifier` is Stripe's own idempotency key — even if our DB
   check were bypassed, Stripe would dedupe.
5. On success, write the audit row:
   `stripe_usage_reports(customer_name, day, kb_reported,
   stripe_event_id, reported_at)`.

On failure: log, no audit row. Tomorrow's run re-tries with the same
`identifier` — if the previous attempt actually succeeded on Stripe's
side but failed before we wrote the audit row, Stripe dedupes.

### Tier changes mid-period

Stripe handles flat-rate proration automatically. For the metered
item the meter-events model is simpler than the legacy usage-records
path: events are posted against the customer, Stripe resolves the
active metered price at ingest time. No column to refresh.

Open question for the event-timing edge: if a customer upgrades from
Swatch → Yard at 14:00 UTC, the day's usage is reported as a single
event at end-of-day with a single timestamp. Stripe attributes the
whole event to whichever price was active at that timestamp (Yard).
That's slightly generous to the customer on the upgrade path (old
Swatch rates wouldn't apply to the day's Swatch-era usage) and
slightly stingy on the downgrade (Swatch rates applied to Yard-era
usage). Acceptable, given daily resolution is our usage granularity.

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
   `store/user.go` using decimal GB (10⁹ bytes); replace literals in
   `handleUsage` and `shellfs.go`. Delete the unreferenced
   `UsageRecord` struct — the reporter introduces a new
   `StripeUsageReport` shape that matches the meter-event audit.
3. ~~Stripe products + prices~~ — done. Meter + three graduated
   metered prices exist in Stripe (see table above).
4. **Extend subscription wiring** — `TierPrices` struct,
   `STRIPE_PRICE_*_OVERAGE` env vars, dual-item subscriptions in
   `handleSetupIntent` / `handleCheckout`. No DB schema change
   (meter-events resolve by customer, not by subscription item).
5. **Build the reporter** — migration 029 creates
   `stripe_usage_reports` (id, customer_name, day, kb_reported,
   stripe_event_id, reported_at; UNIQUE(customer_name, day)). Daily
   03:00 UTC ticker posts `billing/meter_events` per customer with
   idempotency `identifier=weft:<customer>:<date>`.
6. **Surface projected overage** in `/api/billing/usage` and the
   dashboard usage card.

## Open questions — resolved

- **Free-period policy.** Trial subscriptions get no metered item;
  upgrade to a paid tier adds it.
- **Rounding.** Fractional, via Stripe's graduated pricing on the
  metered price. Client reports `floor(bytes / 1000)` KB; Stripe
  handles the rest.
- **Alerting.** A customer about to pay a large overage bill would
  probably rather know in advance. A "you're at 80% of your quota with
  10 days left in the period" email is a useful companion feature —
  out of scope for this doc, but worth flagging so the reporter logs
  enough to feed it.
