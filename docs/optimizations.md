# Unimplemented Optimizations

## 1. Narrow SELECT for compute path

When a query has compute stages, the compute layer only reads `msg.Row.Raw` (`compute/local.go:83`). But the shard SELECT always returns all 10 columns (`line_num, raw, fields, ts, level, app, instance, customer, payload, host_group`) because `ScanRows` (`shard/scan.go:14-59`) does a hardcoded positional scan of exactly those columns.

This means `applyJSONProjection` and `applyFieldProjection` must always select the full column set — they can only rewrite expressions within the fixed schema, not drop columns.

**Waste:** For a pipeline like `tail metrics.log | jq 'select(.type == "memory") | .pct'`, the shard fetches and serializes 8 columns that get thrown away. Only `raw` (and maybe `line_num` for ordering) matter on the compute path.

**Fix:** Give the compute path a stripped-down query mode. `QueryToSink` could use a SELECT that only returns what the compute layer needs, with a matching narrow scanner. The planner would need to signal whether the query is compute-bound (only `raw` needed) or rows-bound (full LogRow needed for merge/streaming).

## 2. Direct shard-to-compute streaming

The proto defines a `QueryToCompute` RPC on `ShardQuery` that would let shards push rows directly to compute workers (shard → compute, bypassing the coordinator as a relay). The shard server currently returns an empty response (`shard/service/server.go:86-90`).

Today all rows flow through the coordinator:

```
Coordinator creates job on compute worker (CreateJob RPC)
  → Shards stream rows to coordinator (QueryToSink)
    → Coordinator forwards to compute worker (SendRows RPC)
      → Coordinator fetches result (GetResult RPC)
```

The optimized path would be:

```
Coordinator creates job on compute worker (CreateJob RPC)
  → Coordinator tells each shard the compute worker address (QueryToCompute RPC)
    → Shards stream rows directly to compute worker (SendRows RPC)
      → Coordinator fetches result (GetResult RPC)
```

This eliminates the coordinator as a data relay, reducing latency and memory pressure on the coordinator for large result sets.

## 3. Schema-aware projection for JSON sources

The planner is schema-unaware (see `docs/architecture-overview.md`, "Gap: the planner is schema-unaware"). For `format: "json"` sources, `raw` is already valid JSON — the `CAST(payload AS TEXT) as raw` rewrite in `applyJSONProjection` is redundant. The narrowed `payload` column (`json_object(...)`) diverges from `raw`, which is wasteful and fragile.

**Fix:** Pass source format metadata into the planner. For `format: "json"` sources, skip JSON projection entirely — `raw` is already the JSON string that jq will consume. The `select` pushdown to SQL is still valuable; only the projection rewrite is unnecessary.
