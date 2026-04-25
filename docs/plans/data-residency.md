# Plan: Data residency (multi-region)

A second deployment of the entire stack in another AWS region, primarily for EU customers who can't have their logs cross the Atlantic. Cell-based: each region is a fully independent copy with its own Postgres, S3, ALB, shards, and coordinator. Customer picks region at signup.

## Why add this

Single-region (us-east-2) is a hard wall for several customer segments:

1. **EU customers under GDPR data-locality requirements.** "We can't ship logs to a US-based service" is a flat-out blocker for some buyers. Schrems II / data-transfer-impact-assessment pressure has not eased.
2. **Latency-sensitive Asia-Pacific customers.** ~250ms RTT to us-east-2 from Sydney is a real UX hit on the interactive shell, separate from any compliance angle.
3. **Sovereignty-conscious public sector and adjacent.** Even where compliance is technically met by a DPA, "the data never leaves my region" is a clean story to tell internal review boards.

The cost is non-trivial — every region is a full second stack to operate — but the alternative is forfeiting this segment forever.

## Architecture

Cell-based, no cross-region data plane:

```
Customer signs up
  → picks region (US / EU at v1 launch; APAC later)
  → DNS routes them to <region>.w3ft.net
  → that region's full stack handles signup, billing, ingest, query

Each region:
  postgres (separate cluster, separate data)
  S3 archive bucket (regional)
  shard ASGs (regional)
  coordinator + api ALBs (regional)
  Stripe integration (regional? — see Open Questions)
```

Decisively *not* federated: a customer in EU has zero data in US, including their account record, billing history, and audit log. Cross-region database joins, replication, or shared control-plane state are off the table. The cost of a unified control plane that we can't actually use to share customer data isn't worth the operational complexity.

The Go binaries are unchanged. The infrastructure is duplicated. Most of the work is in `infrastructure-live/` and `kevcons`.

## What's identical across regions

- All Go code (server, coordinator, shard, weftd, api).
- All Terraform modules (`infrastructure-catalog/`).
- All Packer image build pipelines (kevcons-driven).
- All schemas (Postgres migrations, DuckDB shard schema).
- All web UI (the React build is region-agnostic — region comes from the URL).

## What's unique per region

- A live Terraform stack: `infrastructure-live/prod-eu/weft/` mirrors `infrastructure-live/prod/weft/` with EU-region resources.
- AWS account: separate AWS account per region, or a single account with strict regional resource tagging? Single-account-multi-region is operationally simpler but loses some isolation. Recommend separate accounts at v1 — keeps the blast radius of an IAM mistake bounded.
- DNS: `eu.w3ft.net` and `us.w3ft.net` (or an apex `w3ft.net` that geo-routes to the right region for unauth visitors and the customer's bound region for auth).
- Customer record: every customer has a `region` field set at signup, never updateable in v1.
- agent tokens: `wft_eu_xxx` vs `wft_us_xxx`; the prefix tells weftd which endpoint to hit. Or: tokens are region-opaque and weftd reads `endpoint:` from config (today's behavior). Lean on `endpoint:` and skip the prefix scheme — keeps the auth code in one shape.

## Components to build

1. **`infrastructure-live/prod-eu/weft/`** — full Terraform copy, parameterized on region. Most of the catalog modules already accept a `region` variable; the few that don't get a small refactor.
2. **AWS account provisioning** — separate sub-account in the same Organization for EU. kevcons gains a region-aware `--region` / `--account` flag pair so commands target the right cell.
3. **DNS strategy** — `w3ft.net` apex routes unauthenticated visitors based on geo (Route53 latency-based). Authenticated requests have a `region=eu` cookie or JWT claim and redirect to `eu.w3ft.net`. The signup page is region-aware: the user picks before the first `POST /api/signup`.
4. **Region-aware web client** — `frontend/src/lib/api.ts` reads the region from the JWT and posts to the matching API host. CLI's `weft.yaml` carries `endpoint: eu.w3ft.net:443`.
5. **CI/release pipeline** — GitHub Actions deploy must publish AMIs and updated stacks to *all* active regions on a release. Effectively means "for region in regions: run the existing deploy job."
6. **Customer-region-picker** — signup form gets a region choice with a one-line "this can't be changed later" warning.
7. **Cross-region data-export tool (deferred)** — if/when a customer asks to migrate from US to EU, we'll need an offline export of their archive Parquet + Postgres rows, re-ingested in the destination region. Not v1.

## Open questions

- **Stripe.** Stripe customer/subscription objects are global to a Stripe account. We can either (a) keep one Stripe account and tag customers with region in our metadata; (b) operate two Stripe accounts (US and EU). (a) is simpler and probably correct — Stripe is a payments processor, not a personal-data store, and doesn't trip GDPR data-residency rules in a way the storage-side does.
- **OAuth integrations.** Slack OAuth + PagerDuty integration tokens are stored in Postgres. Per-region storage is fine. But the OAuth redirect URI is region-specific; the Slack app config has to allow both. Solvable, just one more checklist item per region launch.
- **Magic-link email FROM domain.** `paul@littlearkfarm.com` (or whatever the magic-link sender is) is geographically irrelevant; SMTP from a US Mailgun is fine for both regions. Only revisit if a regulator forces email residency.
- **Audit log replication.** If a customer leaves EU and signs up again in US (because we don't migrate), the new account has no audit history of the old one. Acceptable for v1; flagged for compliance review.
- **Shared global services.** The status page, the marketing site, and the docs site stay global. Only the data plane and customer-record control plane go regional.

## Estimated effort

This is mostly infrastructure work, not Go code:

- Terraform parameterization audit: **2 days**
- New AWS account + base infrastructure in EU: **3 days**
- Per-region DNS + cert + routing: **2 days**
- Customer-region picker + JWT region claim: **1 day**
- CI/release pipeline multi-region: **2 days**
- Region-aware client (web + CLI): **1 day**
- Documentation + launch runbook: **2 days**
- First end-to-end EU launch dry-run + bug fixes: **3 days**

**Total: ~16 days (3+ weeks of focused effort)** for the EU launch. Each subsequent region after EU is ~1 week (the Terraform parameterization is amortized; only the new region's infra + DNS + first-launch dry-run remain).

## Non-goals

- **No cross-region replication or failover.** Each region stands alone. If us-east-2 is down, US customers are down; EU customers are unaffected.
- **No data migration between regions.** Customers pick at signup; staying put is the contract. Migration tooling is a separate plan if anyone names it.
- **No global query view.** A US customer cannot see EU shards even if they wanted to. The auth boundary is regional.
- **No dual-region customer accounts.** A customer is in exactly one region. Enterprises with US + EU footprints either run two weft accounts or pick the region that hosts their primary log volume.
- **No "data sovereignty" guarantee beyond AWS region.** We commit to "your data stays in AWS region X." We do not commit to specific availability zones, sub-region facilities, or anything beneath AWS's published region boundary.
