Direct shard-to-compute streaming

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
