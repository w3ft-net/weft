# Plan: ExecuteResult buffered-fields cleanup

## Status

Designed, not started. Test-migration follow-on to commit `8745e98`,
which deliberately kept the buffered fields to avoid a 120-site
test sweep mid-refactor.

## Why

`coordinator.ExecuteResult` is dual-populated:

```go
type ExecuteResult struct {
    Stream  compute.LineStream
    Notices []Notice

    // Legacy buffered view — same data as Stream replays. Kept
    // during phase 4 so the test surface stays stable.
    Rows    []*pb.ResultRow
    Text    string
    Stderr  string
}
```

`Stream` is the primary surface — the gRPC server pumps it directly
onto the wire. `Rows / Text / Stderr` exist solely so ~120
pre-streaming test field accesses keep compiling. They're a slice
view of the same data the stream replays.

This is a transitional cost. Two reasons to clean it up:

1. **Conceptual clarity.** "ExecuteResult has both a stream and a
   buffered view" is confusing. Half-streaming, half-batch is the
   exact shape we replaced QueryResult to escape.
2. **Memory.** For large queries the buffered fields hold the
   entire result alongside the slice replay. Stream consumers pay
   for memory they never read.

## What changes

### Test helper

Add a single helper in a new `coordinator/result_drain_helper_test.go`:

```go
package coordinator

import (
    "context"
    "testing"
)

// drainExec drains an ExecuteResult into the legacy buffered shape
// for tests that assert against Rows/Text/Stderr. Production
// callers (the gRPC server) consume Stream directly and do not
// call this helper.
func drainExec(t *testing.T, res *ExecuteResult) *DrainedResult {
    t.Helper()
    if res == nil {
        return nil
    }
    d, err := res.Drain(context.Background())
    if err != nil {
        t.Fatalf("drain ExecuteResult: %v", err)
    }
    return d
}
```

Where `DrainedResult` is added to `coordinator/coordinator.go`:

```go
type DrainedResult struct {
    Rows    []*pb.ResultRow
    Text    string
    Stderr  string
    Notices []Notice
}

func (r *ExecuteResult) Drain(ctx context.Context) (*DrainedResult, error) {
    out := &DrainedResult{Notices: r.Notices}
    var stderrSb strings.Builder
    for {
        line, ok, err := r.Stream.Next(ctx)
        if err != nil {
            return out, err
        }
        if !ok {
            break
        }
        if line.Stderr {
            stderrSb.WriteString(line.Raw)
            stderrSb.WriteByte('\n')
            continue
        }
        out.Rows = append(out.Rows, &pb.ResultRow{
            Raw: line.Raw, LineNum: line.LineNum,
        })
    }
    var textSb strings.Builder
    for _, row := range out.Rows {
        textSb.WriteString(row.Raw)
        textSb.WriteByte('\n')
    }
    out.Text = textSb.String()
    out.Stderr = stderrSb.String()
    return out, nil
}
```

### Test sweep

For each test that reads `result.Rows / Text / Stderr`, insert a
drain call after the Execute. Pattern:

```go
// before:
result, err := c.Execute(`cat vimgym.log`, ...)
if err != nil { t.Fatal(err) }
if len(result.Rows) != 16 { ... }

// after:
res, err := c.Execute(`cat vimgym.log`, ...)
if err != nil { t.Fatal(err) }
result := drainExec(t, res)
if len(result.Rows) != 16 { ... }
```

The variable rename (`result` → `res` for the ExecuteResult, then
`result` becomes the DrainedResult) keeps subsequent
`result.Rows` accesses unchanged.

Files touched (per `grep -rln "result\.Text\|result\.Rows\|result\.Stderr\|result\.Notices\|res\.Text\|res\.Rows\|res\.Stderr\|res\.Notices" --include='*_test.go' coordinator/`):

- `archiver_test.go`
- `range_pushdown_test.go`
- `grpc_integration_test.go`
- `bin_interior_query_test.go`
- `coordinator_restart_test.go`
- `namespace_test.go`
- `alert_customer_resolve_test.go`
- `cron_bin_test.go`
- `coordinator_test.go`
- `cat_redirect_tmp_test.go`
- `alert_notifier_test.go`
- `script_timeout_test.go`
- `compute_unification_test.go`
- `execute_test.go`
- `alert_test.go`
- `bin_exec_test.go`
- `project_test.go`
- `failover_test.go`

~120 field accesses across 18 files. Mechanical sweep. Use a
scripted refactor (e.g. `gopls rename` per accessor or a sed pass
that handles the assignment-then-reference pattern) rather than
hand-editing each.

### Field deletion

Once tests build green via the helper:

```go
// coordinator/coordinator.go
type ExecuteResult struct {
    Stream  compute.LineStream
    Notices []Notice
    // Rows, Text, Stderr — DELETED
}

func queryResultToExecuteResult(qr *QueryResult) *ExecuteResult {
    var lines []compute.Line
    // ... build lines from qr.Rows / qr.Text / qr.Stderr ...
    return &ExecuteResult{
        Stream:  compute.NewSliceLineStream(lines, nil),
        Notices: qr.Notices,
        // Rows: qr.Rows, etc. — DELETED
    }
}
```

`QueryResult` stays as the internal coord-side buffered shape
(used by `c.query`, `executeQuery`, `runLocal`, alert/redirect
post-processing). Public callers see only `ExecuteResult` /
`DrainedResult`.

## Testing

Existing tests pass after the migration. No new tests needed —
the helper is the test infrastructure.

After the field deletion, `go vet ./coordinator/` should be clean
and `go test ./coordinator/` should be green.

## Risk

- **Tests that mix Stream and buffered access.** Rare, but if any
  exist (e.g. a test that calls `Stream.Next` and ALSO reads
  `result.Rows`), the migration loses one of the two views. The
  Stream is single-consumer; calling Drain consumes it.

  Audit: `grep -rn 'Stream\.Next\|\.Stream' --include='*_test.go' coordinator/` should turn up only the helper itself.

- **Goroutine ownership.** Any test that hands the ExecuteResult to
  another goroutine (e.g. via `go func() { ... result.Rows ... }()`)
  needs the drain to complete before the goroutine reads. The
  helper drains synchronously, so this is fine — but worth noting
  if any test uses background result-reading.

## Estimated effort

- Helper + Drain method: ~50 lines. Half an hour.
- Test sweep: ~120 sites across 18 files. Bulk pattern is
  mechanical; manually verify each file. ~3-4 hours.
- Field deletion + rebuild: ~30 minutes including running the
  whole test suite.
- Total: ~half a day.

## Non-goals

- **Migrating non-test callers.** The gRPC server already consumes
  Stream. CLI / web shell still go through the wire (no direct
  ExecuteResult access). No production callers read the buffered
  fields.
- **Eliminating QueryResult.** QueryResult stays as the internal
  coord-side type. Only the public ExecuteResult shape sheds the
  duplicate fields.
- **Changing the Drain semantics.** The current `Drain(ctx)` shape
  is preserved — same return type, same error model.

## Cross-link

- Commit `8745e98` (Coordinator.Execute returns *ExecuteResult)
  for context on why the dual population exists.
- `end-to-end-streaming.md` step 2 — when the streaming path
  forwards compute's stream directly, it won't pre-populate the
  buffered fields anyway. This cleanup makes that absence the
  uniform shape rather than a special case.
