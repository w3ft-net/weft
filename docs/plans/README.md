# Plans

Design docs for upcoming work. Each plan is self-contained — read it
before touching the affected code. This README is the index plus a
suggested build order; individual plans are authoritative on their
own designs.

Security & compliance plans are grouped under
[`compliance/`](compliance/README.md) — the audit trail, ingest- and
read-time redaction, and the cross-tenant disclosure finding. They keep
their roadmap phasing below; the subfolder README is the umbrella that
ties them together.

## Status

| Plan | State |
|---|---|
| Shipped plans live in `docs/completed/`. Recent additions: `weftd-authored-config`, `loom-producer-host`, `python-sdk`, `anomaly-conditions` (shipped as the `weft::is_outlier`/`weft::is_seasonal` predicates), `query-sharing`, `dashboard-builder`, `onboarding`, `oidc`, `cross-shard-post-limit-filter`, `executeresult-buffered-cleanup`, `shard-grpc-resilience` (Phase 3 deferred), `notify-destinations`, `migration-collapse`. |
| `weftd-client-side-parsing` — **default-on**; `WEFTD_PARSE_CLIENT_SIDE=0` is now the rollback lever. Stays in this dir until the flag is removed. |
| Everything in this directory | 🟡 Designed, not started (or partially shipped — read the plan's own Status section) |

## Ease vs. value ranking

A second lens on the same plans, orthogonal to the dependency-ordered
Roadmap below. `Ease` and `Value` are 1–5 (5 = easiest / highest
value). Effort is each plan's own estimate. The Roadmap is the
build order; this table is for picking opportunistic wins.

| Plan | Effort | Ease | Value | Note |
|---|---:|:--:|:--:|---|
| [weftd-client-side-parsing](weftd-client-side-parsing.md) | shipped | 5 | 4 | Default-on; flag is now a rollback lever only |
| [chart-pipe-stage-axis-labels](chart-pipe-stage-axis-labels.md) | 0.5d | 5 | 2 | Trivial but deferred — no demand |
| [streaming-backpressure](streaming-backpressure.md) | 0.5d | 5 | 2 | No production trigger yet |
| [compute-scaling](compute-scaling.md) | ~1d | 5 | 2 | Placeholder; defer until CPU saturates |
| [nested-query-compute-pool](nested-query-compute-pool.md) | ~1d (phase A) | 4 | 3 | Ship save-time validation first |
| [log-to-metric](log-to-metric.md) | 1.5d | 4 | 4 | Cheap rider on ingest-governance |
| [perf-hints](perf-hints.md) | 1.5d | 4 | 3 | Polish |
| [http-ingest](http-ingest.md) | ~2d left | 4 | 5 | Heroku shipped; JSON/CloudWatch adapters remain |
| [render](render.md) | ~5d (+1d relay) | 3 | 4 | Prospect-driven; TLS syslog listener on the existing NLB. Relay fallback ships first as the unblock |
| [shard-archive-test](shard-archive-test.md) | ~2–3d | 4 | 3 | Operational validation only |
| [weftd-multiline](weftd-multiline.md) | 3d | 4 | 4 | Easiest high-ROI win |
| [cloudwatch-lambda-forwarder](cloudwatch-lambda-forwarder.md) | ~3d | 4 | 4 | |
| [alert-silencing](alert-silencing.md) | 3.5d | 4 | 5 | |
| [weftd-disk-spool](weftd-disk-spool.md) | 3.5d | 3 | 4 | |
| [cross-tenant-coordinator-log](compliance/cross-tenant-coordinator-log.md) | ~1–2d* | 3 | 5 | *Triage-gated; live security defect |
| [bucket-split-policy](bucket-split-policy.md) | 3–4d | 3 | 4 | Fixes a broken/disabled feature |
| [alert-multirow-tier-limits](alert-multirow-tier-limits.md) | 3–4d | 3 | 4 | Blocked on real customer data |
| [streaming-parallel-warp](streaming-parallel-warp.md) | 4d | 3 | 4 | |
| [ingest-governance](compliance/ingest-governance.md) | 4.5d | 3 | 5 | |
| [user-sso](user-sso.md) | 4.5d | 3 | 5 | |
| [retention-enforcement](retention-enforcement.md) | ~4–5d | 3 | 5 | |
| [audit-log](compliance/audit-log.md) | 4.75d | 3 | 5 | |
| [field-redaction](compliance/field-redaction.md) | 5d | 3 | 4 | |
| [weftd-k8s](weftd-k8s.md) | 7d | 2 | 5 | Needs ingest-dimensions first |
| [aws-source-presets](aws-source-presets.md) | 8d | 2 | 5 | |
| [byok-encryption](byok-encryption.md) | 8d | 2 | 5 | |
| [watch-lowering-followups](watch-lowering-followups.md) | ~8d | 2 | 3 | Splittable; nested-bin/ subset is 2d |
| [materialized-views](materialized-views.md) | ~8–10d | 2 | 5 | Architecture-heavy, open questions |
| [coverage-improvement](coverage-improvement.md) | ~12.5d | 1 | 4 | PR-delta subset is a smaller standalone win |
| [data-residency](data-residency.md) | ~16d | 1 | 4 | Mostly repeatable infra |
| [az-resilience](az-resilience.md) | multi-week | 1 | 4 | Deferred pending infra maturity |

**Sweet spot (high value, low effort), in order:** http-ingest →
alert-silencing → weftd-multiline → ingest-governance (+ log-to-metric
rider). Run the [cross-tenant-coordinator-log](compliance/cross-tenant-coordinator-log.md)
triage independently of this ordering — it is a live security defect.

The expensive-but-unavoidable tier (byok-encryption, weftd-k8s,
data-residency, materialized-views) is all Value 4–5; schedule those
deliberately, not opportunistically.

## Roadmap

A dependency-ordered phasing for the user-facing / commercial set.
Effort estimates are rough and per-plan; total runs ~85 person-days.

### Phase 0 — foundational

Each unblocks a meaningful slice of downstream work.

| Plan | Effort | Why first |
|---|---:|---|
| [http-ingest](http-ingest.md) | ~2d remaining | Heroku adapter shipped; JSONAdapter (`/api/ingest`) + CloudWatchAdapter still pending |
| [audit-log](compliance/audit-log.md) | 4.75d | Cheap to wire once before five downstream plans want to emit events |

(`python-sdk` shipped — moved to `docs/completed/`. It was built as
the standalone sibling repo `weft-py`, not `clients/python/`.)

### Phase 1 — table-stakes commercial

Things customers will name in the first sales call.

| Plan | Effort | Notes |
|---|---:|---|
| [user-sso](user-sso.md) | 4.5d | OIDC for Google + generic; SAML/SCIM deferred |
| [ingest-governance](compliance/ingest-governance.md) | 4.5d | Ordered drop / sample / redact rules at the drain |
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
| [composite-alerts](../completed/composite-alerts.md) | 2.25d | Mostly docs + `weft.combine` helper |

(`anomaly-conditions` shipped — moved to `docs/completed/`. Landed as
the `weft::is_outlier` / `weft::is_seasonal` predicates rather than
`alert outlier`/`alert seasonal` grammar.)

### Phase 4 — enterprise dealbreakers

Each one a buying-blocker for some procurement team.

| Plan | Effort | Notes |
|---|---:|---|
| [field-redaction](compliance/field-redaction.md) | 5d | Read-time per-role masking |
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
| [coverage-improvement](coverage-improvement.md) | Per-PR coverage delta, per-package floors, eliminating the `-p 1` test-isolation workaround |

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
