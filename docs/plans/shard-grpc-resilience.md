# Root Cause Analysis: Transient gRPC Errors After Coordinator+Compute Restart

## Executive Summary

After a coordinator restart, queries fail transiently with `code = Canceled desc = grpc: the client connection is closing`. The root cause is **address flapping**: when the coordinator comes back online, the provisioner's `ProcessHeartbeat` handler rapidly rebinds each shard's gRPC connection multiple times in succession (lines 444–456 of `/Users/paul/dev/weft/coordinator/provisioner.go`). Meanwhile, concurrent queries hold references to `shard.Shard` objects whose underlying `*grpc.ClientConn` gets closed and replaced mid-flight, causing in-flight RPC streams to be cancelled by the gRPC runtime.

The symptom (`sentinels=17 by_shard=<15 entries>`) confirms the problem: 1–2 shards' streaming responses never reached the compute job because the gRPC stream was cut while sending.

## Root Cause Analysis

### 1. Why Shard Addresses Flap After Coordinator Restart

**File:** `/Users/paul/dev/weft/coordinator/provisioner.go:398–411` (adoptFromHeartbeats)

When the provisioner starts:
1. It calls `adoptFromHeartbeats()` (line 218 in provisioner.go)
2. This fetches all heartbeats from the last 2 minutes (line 403)
3. For each heartbeat, it calls `ProcessHeartbeat()` directly (line 410)

Then, on every ticker interval (default 5s for production, 5s for dev):
4. `adoptFromHeartbeats()` runs again (line 232), re-fetching heartbeats

**Immediately after restart**, the coordinator's in-memory registry is empty. The shard containers themselves are still running and have been sending heartbeats the whole time. When the coordinator comes back:
- Heartbeat #1 arrives for `shard-18 at container-A:9000` → adopted into registry
- Heartbeat #2 arrives for `shard-18 at container-B:9000` → **rebind triggered** (line 437–456)
- Heartbeat #3 arrives for `shard-18 at container-C:9000` → **rebind triggered again**

The shards themselves are NOT moving—their containers are stable. The address flapping happens because:

**File:** `/Users/paul/dev/weft/cmd/shard/main.go:runHeartbeat()`

Each shard sends its heartbeat every ~10s (typical interval). If the coordinator restarts while a shard's heartbeat is in flight, that heartbeat (from the old container's IP) may be discarded. The shard then sends a new one with its current container address. The provisioner processes both in rapid succession, triggering multiple rebinds on a stable shard.

**Additionally, in Docker Compose**, when containers are restarted, the IP address in Docker's internal DNS may be reassigned even if the container name stays the same. This compounds the problem: the provisioner sees what appears to be the same shard moving between IPs in successive heartbeats.

### 2. The Rebind-and-Close Race Condition

**File:** `/Users/paul/dev/weft/coordinator/provisioner.go:444–456` (ProcessHeartbeat rebind path)

```go
// Line 437: Check if address changed
if existing.Addr == hb.Addr {
    return // No change, exit early
}

// Lines 445–449: Create new connection to new address
gs, err := shard.NewGRPCShard(hb.ShardID, hb.Addr, p.config.ShardSecret)
if err != nil {
    return
}

// Lines 451–453: CLOSE OLD, INSTALL NEW
existing.Shard.Close()         // ← Closes the old *grpc.ClientConn
existing.Shard = gs            // ← Installs new connection
```

**The problem:** While these lines execute under `heartbeatMu` lock (line 425), queries that already hold a reference to `existing.Shard` (the old `GRPCShard` object) will call methods on the **already-closed connection**.

**File:** `/Users/paul/dev/weft/coordinator/coordinator.go:530–542` (fan-out query path)

```go
for _, s := range matching {
    go func(s shard.Shard) {
        defer wg.Done()
        rows, err := s.Query(directSQL)  // ← s is a shard.Shard interface
        // ...
    }(s)
}
```

The goroutine captures `s` by value, but `s` is an interface wrapping a `*GRPCShard`. If the provisioner's rebind closes the underlying connection while this RPC is in flight:
1. The `*grpc.ClientConn` transitions to `TRANSIENT_FAILURE` state
2. gRPC cancels all pending RPCs with `code = Canceled`
3. The RPC returns `grpc: the client connection is closing`

### 3. Missing Per-RPC Context Cancellation (But That's Not the Root)

gRPC clients normally would use a context with a timeout. However:

**File:** `/Users/paul/dev/weft/shard/grpc_client.go:72–76` (Query)

```go
stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
if err != nil {
    return nil, fmt.Errorf("query shard %s: %w", s.id, err)
}
```

Uses `context.Background()` with **no timeout**. This is actually correct for long-running queries, but it means cancellation only comes from:
- The coordinator cancelling the context (doesn't happen here)
- The gRPC connection closing (our problem)

### 4. Streaming Queries Don't Recover Gracefully

**File:** `/Users/paul/dev/weft/shard/grpc_client.go:92–113` (QueryToSink for compute)

```go
stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
if err != nil {
    log.Printf("shard %s: QueryToSink stream error: %v (sql=%s)", s.id, err, sql)
    return  // ← Returns WITHOUT SendSentinel on error path
}

for {
    row, err := stream.Recv()
    if err == io.EOF {
        break
    }
    if err != nil {
        log.Printf("shard %s: QueryToSink recv error: %v", s.id, err)
        return  // ← Returns WITHOUT SendSentinel on recv error
    }
    // ...
}
// defer SendSentinel only runs if we exit normally
```

Actually, line 94 has `defer sink.SendSentinel(s.id)`, so the sentinel IS sent. But the log entry at line 98 and 108 shows when errors happen—and if the stream fails mid-way through, the job's `consume()` loop is still waiting for that sentinel. However, the sentinel WILL arrive because of the defer.

**The real issue:** The stream error happens (connection closing), the sentinel arrives late, but other shards' streams may have already succeeded. The compute job receives sentinel out of order or with incomplete data.

**File:** `/Users/paul/dev/weft/compute/local.go:80–100` (consume)

```go
for sentinels < j.shardCount {
    msg := <-j.ch
    if msg.Sentinel {
        sentinels++
        continue
    }
    if msg.Row == nil {
        continue
    }
    if j.mergeKey == "" {
        lines = append(lines, msg.Row.Raw)
    } else {
        perShard[msg.ShardID] = append(perShard[msg.ShardID], msg.Row)
    }
}
```

If a shard stream fails and NO rows are buffered for it, the sentinel arrives, but zero rows have been collected. The output is incomplete.

## Analysis of Concrete Questions

### (a) Is the fundamental issue shard-side flapping, stale connection caching, or something else?

**Answer: (c) Something else — connection replacement mid-flight + rebind race.**

The shards themselves are NOT flapping. The coordinator is:
1. Receiving multiple heartbeats from a stable shard (because the coord was down)
2. Treating each unique address as a new location, triggering a rebind
3. Closing the old connection and installing a new one while queries hold references to the old interface

### (b) What's the right fix shape?

**Answer: Multiple fixes, in priority order:**

1. **Defer-close the old connection after releasing the lock** (immediate, lowest risk)
   - File: `/Users/paul/dev/weft/coordinator/provisioner.go:444–456`
   - After `existing.Shard = gs` (line 452), don't call `existing.Shard.Close()` immediately
   - Store the old shard in a local var, release `heartbeatMu` lock, then close
   - This allows in-flight queries to complete before the connection dies

2. **Coalesce rapid rebinds** (medium complexity, higher impact)
   - Add a "last rebind timestamp" field to ShardEntry
   - If a rebind happened <1s ago and the new address is different from the last one, defer the rebind and log it as suspicious
   - This prevents the cascading address swaps during coordinator startup

3. **Add per-query retry with backoff** (medium complexity, makes the system more resilient)
   - File: `/Users/paul/dev/weft/shard/grpc_client.go:72–90` (Query)
   - Wrap the gRPC call in a retry loop: on `Canceled`, wait 100ms and try again (up to 3 attempts)
   - For streaming queries (QueryToSink), only retry on the initial `client.Query()` call, not mid-stream
   - This handles transient connection flaps gracefully

4. **Use wait-for-ready on new connections** (low risk, incremental improvement)
   - When creating a new GRPCShard connection, pass `grpc.WithDefaultCallOptions(grpc.WaitForReady(true))`
   - Tells gRPC to wait for the connection to be ready before sending the first RPC
   - Won't fix the close-during-RPC issue, but will prevent some race conditions

### (c) Should MatchingShards or ShardRegistry consider connection health?

**Answer: No, not in ShardRegistry. Instead:**

- `MatchingShards` returns `shard.Shard` interfaces that are assumed healthy
- The issue is not selection—it's connection lifecycle during rebind
- Checking health would add overhead and wouldn't fix the race
- The better approach: **stabilize the address before creating new connections**

### (d) Where should retry logic live?

**Answer: At the gRPC client level** (`/Users/paul/dev/weft/shard/grpc_client.go`), **not** at the fan-out layer.

- **Reason 1:** The fan-out layer is already complex (coordinator.go:530–542, compute job aggregation)
- **Reason 2:** A single shard's RPC failing should be retried before propagating up to the query
- **Reason 3:** The gRPC `Canceled` error is a low-level transport issue; retrying there is idiomatic

**Concrete location:**

```go
// In shard/grpc_client.go, Query() method
func (s *GRPCShard) Query(sql string) ([]*pb.ResultRow, error) {
    const maxAttempts = 3
    const retryBackoff = 100 * time.Millisecond
    
    var lastErr error
    for attempt := 0; attempt < maxAttempts; attempt++ {
        stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
        if err == nil {
            // Recv loop here as before
        }
        lastErr = err
        if !isCancelledOrConnectionError(err) {
            return nil, fmt.Errorf("query shard %s: %w", s.id, err)
        }
        // On Canceled or connection error, backoff and retry
        if attempt < maxAttempts-1 {
            time.Sleep(retryBackoff)
        }
    }
    return nil, fmt.Errorf("query shard %s (after %d attempts): %w", s.id, maxAttempts, lastErr)
}

func isCancelledOrConnectionError(err error) bool {
    // Check for "connection closing" or grpc.codes.Canceled
    s, ok := status.FromError(err)
    if ok && (s.Code() == codes.Canceled || s.Code() == codes.Unavailable) {
        return true
    }
    return strings.Contains(err.Error(), "connection closing")
}
```

### (e) How should follow/streaming queries behave differently?

**Answer: Same retry logic, but only on initial stream creation, not mid-receive.**

For `QueryToSink` (used by compute jobs):
- Retry the initial `s.client.Query()` call (lines 96, before the recv loop)
- Do NOT retry mid-stream — once rows are flowing, a mid-stream error means the shard genuinely failed and we should fail fast
- The `defer sink.SendSentinel()` ensures the compute job doesn't hang

**Rationale:**
- If the stream fails before any rows, retrying may recover
- If the stream fails mid-way, retrying would mean re-executing the query on the shard, which may produce duplicate rows
- The sentinel mechanism already handles shard failures gracefully for the compute aggregator

## Recommended Implementation Plan

### Phase 1: Immediate (1–2 hours, critical)

1. **Defer-close old connections in ProcessHeartbeat**
   - File: `/Users/paul/dev/weft/coordinator/provisioner.go:444–456`
   - Change line 451 from `existing.Shard.Close()` to storing the old shard, releasing the lock, then closing outside the critical section
   - This prevents the close-during-RPC race

2. **Add logging to detect rapid rebinds**
   - Track rebind timestamps per shard
   - Log a warning if >1 rebind in 5 seconds
   - This will surface the startup cascade issue and help with debugging future flaps

### Phase 2: Short-term (2–4 hours, high-value)

3. **Implement retry logic in GRPCShard.Query() and QueryToSink()**
   - File: `/Users/paul/dev/weft/shard/grpc_client.go:72–113`
   - Add 3-attempt retry with 100ms backoff on `Canceled` errors
   - Only retry initial stream creation, not mid-recv
   - This makes queries resilient to transient connection flaps

### Phase 3: Medium-term (4–8 hours, nice-to-have)

4. **Add wait-for-ready on new connections**
   - File: `/Users/paul/dev/weft/coordinator/provisioner.go:156` (grpcConfigurer.Dial)
   - Pass `grpc.WithDefaultCallOptions(grpc.WaitForReady(true))` to ensure the connection is usable before attaching the first RPC

5. **Coalesce rapid rebinds**
   - File: `/Users/paul/dev/weft/coordinator/provisioner.go:437–443`
   - If a shard has been rebindable within the last 2 seconds and the new address differs from the last address, defer the rebind
   - Log it as "shard flapping" so on-call can investigate whether Docker DNS is unstable

## Files and Line References for Implementation

| File | Lines | Change |
|------|-------|--------|
| `/Users/paul/dev/weft/coordinator/provisioner.go` | 444–456 | Defer-close old connection |
| `/Users/paul/dev/weft/coordinator/provisioner.go` | 437–443 | Add rapid-rebind coalescing |
| `/Users/paul/dev/weft/shard/grpc_client.go` | 72–90 | Add retry logic to Query() |
| `/Users/paul/dev/weft/shard/grpc_client.go` | 93–113 | Add retry logic to QueryToSink() |
| `/Users/paul/dev/weft/coordinator/provisioner.go` | 156 | Add wait-for-ready option to NewGRPCShard call |

## Testing

1. **Add a test that simulates heartbeat arrival during queries**
   - Start a coordinator with a shard
   - Begin a slow query (add artificial delay in the shard query handler)
   - While the query is in flight, inject heartbeats with different addresses
   - Verify the query completes instead of returning `Canceled`

2. **Add a test for rapid rebinds**
   - Send 3–4 heartbeats for the same shard with different addresses in quick succession
   - Verify only the last address is active (after coalescing)
   - Verify no queries are cancelled mid-flight

3. **Existing test:** `coordinator_restart_test.go` already covers the happy path; extend it to interleave queries during heartbeat adoption.

## Risk Assessment

**Phase 1 (defer-close):** Very low risk. Moving the close call outside the critical section is a straightforward synchronization fix.

**Phase 2 (retry logic):** Low-to-medium risk. Retries are idiomatic in gRPC, but need to be careful not to retry forever or introduce unexpected delays. The 3-attempt limit with 100ms backoff is conservative.

**Phase 3 (wait-for-ready, coalescing):** Medium risk. Both are reasonable improvements but less critical than phases 1–2. Can be deferred if timeline is tight.

## Validation

Once implemented, reproduction should fail:
1. Restart coordinator+compute (shard containers stay running)
2. Run queries: should complete reliably
3. Check logs: should see zero or one "shard moved" log per shard, not 4+
4. Monitor: `sentinels` should equal `by_shard entries` on every query
