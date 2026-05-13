# Plans

Design docs for upcoming work. Each plan is self-contained — read it
before touching the affected code. This README is the index plus a
suggested build order; individual plans are authoritative on their
own designs.

## Status

| Plan | State |
|---|---|
| Shipped plans live in `docs/completed/`. Recent additions: `query-sharing`, `dashboard-builder`, `onboarding`, `oidc`, `cross-shard-post-limit-filter`, `executeresult-buffered-cleanup`, `shard-grpc-resilience` (Phase 3 deferred), `notify-destinations`, `migration-collapse`. |
| Everything in this directory | 🟡 Designed, not started (or partially shipped — read the plan's own Status section) |

## Roadmap

A dependency-ordered phasing for the user-facing / commercial set.
Effort estimates are rough and per-plan; total runs ~85 person-days.

### Phase 0 — foundational

Each unblocks a meaningful slice of downstream work.

| Plan | Effort | Why first |
|---|---:|---|
| [http-ingest](http-ingest.md) | ~2d remaining | Heroku adapter shipped; JSONAdapter (`/api/ingest`) + CloudWatchAdapter still pending |
| [python-sdk](python-sdk.md) | 3.5d | Python-flavored runtime stats. Pairs with the now-shipped go-sdk. |
| [audit-log](audit-log.md) | 4.75d | Cheap to wire once before five downstream plans want to emit events |

### Phase 1 — table-stakes commercial

Things customers will name in the first sales call.

| Plan | Effort | Notes |
|---|---:|---|
| [user-sso](user-sso.md) | 4.5d | OIDC for Google + generic; SAML/SCIM deferred |
| [ingest-governance](ingest-governance.md) | 4.5d | Ordered drop / sample / redact rules at the drain |
| [log-to-metric](log-to-metric.md) | 1.5d | Tiny extension of governance; ship together |

### Phase 2 — operational polish

Quality-of-life that compounds, mostly independent.

| Plan | Effort | Notes |
|---|---:|---|
| [aws-source-presets](aws-source-presets.md) | 8d | 10 AWS schemas (VPC Flow, CloudTrail, ALB, …); per-preset fixture work |
| [alert-silencing](alert-silencing.md) | 3.5d | Maintenance windows + mute schedules |
| [weftd-disk-spool](weftd-disk-spool.md) | 3.5d | Outage survivability for the agent |
| [weftd-multiline](weftd-multiline.md) | 3d | Stack-trace stitching at the agent |

### Phase 3 — visibility + smarter alerting

| Plan | Effort | Notes |
|---|---:|---|
| [anomaly-conditions](anomaly-conditions.md) | 2.75d | `outlier`, `seasonal` alert conditions |
| [composite-alerts](composite-alerts.md) | 2.25d | Mostly docs + `weft.combine` helper |

### Phase 4 — enterprise dealbreakers

Each one a buying-blocker for some procurement team.

| Plan | Effort | Notes |
|---|---:|---|
| [field-redaction](field-redaction.md) | 5d | Read-time per-role masking |
| [weftd-k8s](weftd-k8s.md) | 7d | DaemonSet via Helm; depends on `ingest-dimensions` |
| [byok-encryption](byok-encryption.md) | 8d | Customer KMS keys for the Parquet archive |
| [data-residency](data-residency.md) | 16d | Cell-based multi-region; one EU launch |

## Dependency map

`ingest-dimensions` and `notify-destinations` already shipped; the
remaining hard prereqs are:

```
        http-ingest ────────┐
                            │
                     ingest-governance ──┬─→ log-to-metric
                            │            └─→ weftd-k8s
                            │
                     audit-log ──── (referenced by governance,
                                     silencing, redaction, sso, byok)

                     alert-silencing  (independent of notify-destinations
                                       now that it shipped)
```

Everything else is independent and parallelizable.

## Operational / infra plans

These existed before the dimension/dimensions roadmap and live on
their own clocks. They're sequenced by operational pressure rather
than product strategy.

| Plan | What |
|---|---|
| [az-resilience](az-resilience.md) | Multi-AZ shard placement |
| [bucket-split-policy](bucket-split-policy.md) | When to split a multi-tenant shard bucket |
| [retention-enforcement](retention-enforcement.md) | Real deletion behind the per-plan cap (today the cap is filter-only) |
| [materialized-views](materialized-views.md) | Save a pipeline as a continuously-updated view table |
| [direct-to-compute](direct-to-compute.md) | Shard streams rows directly to compute (skip coordinator) |
| [nested-query-compute-pool](nested-query-compute-pool.md) | Compute calling back into the coordinator for sub-queries |
| [compute-scaling](compute-scaling.md) | When the compute pool needs to scale beyond 1 |
| [cloudwatch-lambda-forwarder](cloudwatch-lambda-forwarder.md) | Push-based CloudWatch alternative to today's poll |
| [perf-hints](perf-hints.md) | Performance optimization opportunities |
| [alert-multirow-tier-limits](alert-multirow-tier-limits.md) | Per-tier caps on multi-row alert fan-out |
| [shard-archive-test](shard-archive-test.md) | Integration test harness for cold-shard rehydration |
| [streaming-backpressure](streaming-backpressure.md) | Bounded per-shard queues + configurable streamCh; defer until production hits memory pressure |
| [streaming-parallel-warp](streaming-parallel-warp.md) | Combine streaming + chunked intra-stage parallelism in warp |

## Conventions for new plans

- Filename matches the plan's noun: `feature-name.md`. No dates.
- Lead with **Why** — the problem, not the solution.
- **Architecture** with a small diagram or shape of the change.
- **Components to build** as a numbered list with file paths.
- **Estimated effort** broken out per component.
- **Non-goals** — what we're explicitly not doing in this plan.
- Cross-link to other plans this depends on or supersedes.

When a plan ships, mark it ✅ in the Status table above. Plans
that get implemented may stay in this directory as design provenance
(see `ingest-dimensions.md`) or get removed if they no longer
provide useful context.
