package compute

import (
	"context"
	"log"
	"sync"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// ComputeMessage is sent from shards to a compute job. Either Row is set
// (a data row) or Sentinel is true (indicating the shard has finished).
type ComputeMessage struct {
	Row      *pb.ResultRow
	ShardID  string
	Sentinel bool
}

// ComputePool creates compute jobs. Implementations:
//   - LocalComputePool: in-process execution via warp's bash interpreter
//   - GRPCComputePool: fans out to remote compute workers
//
// keepLast selects the limit semantic: false is the default head-style
// "stop after N emitted"; true switches to tail-style "retain last N,
// emit at EOF" — required when shards return ASC after the tail
// subquery flip (see planner/sql.go::Query.Wrap).
//
// traceTag, when true, switches the job's zero-stage output from plain
// streaming to buffer-then-summarize-then-tag: every row is prefixed
// with a colored [namespace/service] badge and a synthetic summary line
// precedes the batch. Only ever set true for a bare, interactive
// `trace <id>` — see coordinator.go's executeQuery.
type ComputePool interface {
	NewJob(stages []string, shardCount int, mergeKey string, limit int, keepLast bool, traceTag bool) ComputeJob

	// ExecuteQuery fans out SQL to the given shards, merges results,
	// applies pipeline stages, and streams output lines back. ctx
	// carries the ambient trace ID (if any) across the gRPC boundary
	// to a remote compute worker/shard — see weftsdk.ContextWithTraceID.
	ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (LineStream, error)
}

// ResolveShardFunc materializes a shard.Shard handle for a descriptor.
// Implementations decide whether to dial fresh, return a cached
// handle, or consult a registered Resolver override. FanOutToShards
// is otherwise oblivious to where the handle came from — the boundary
// is "give me something I can call QueryToSink on."
type ResolveShardFunc func(d *pb.ShardDescriptor) (shard.Shard, error)

// FanOutToShards turns each descriptor into a handle (via resolve),
// runs QueryToSink concurrently, and waits. The resolver owns
// connection caching and rebind invalidation; FanOut is intentionally
// dumb about lifecycle — it does not Close the handle on return,
// because the pool's cache may keep it for the next query.
func FanOutToShards(ctx context.Context, req *pb.ExecuteQueryRequest, job ComputeJob, resolve ResolveShardFunc) {
	args := make([]any, len(req.Args))
	for i, a := range req.Args {
		args[i] = a
	}
	var wg sync.WaitGroup
	wg.Add(len(req.Shards))
	for _, desc := range req.Shards {
		go func(d *pb.ShardDescriptor) {
			defer wg.Done()
			sh, err := resolve(d)
			if err != nil {
				log.Printf("execute-query: resolve shard %s: %v", d.ShardId, err)
				job.SendSentinel(d.ShardId)
				return
			}
			sh.QueryToSink(ctx, req.Sql, args, job)
		}(desc)
	}
	wg.Wait()
}

// ComputeJob receives rows from shards and produces output as a stream
// of lines. It implements shard.RowSink so shards can stream directly
// to it.
type ComputeJob interface {
	shard.RowSink
	// Send delivers a row or sentinel from a shard. Safe for concurrent use.
	Send(msg ComputeMessage)
	// Stream returns a forward-only line stream of the job's output.
	// Multiple calls return independent views over the same underlying
	// result; the underlying pipeline runs once.
	Stream() LineStream
	// Result blocks until all sentinels are received and the pipeline has
	// finished executing, then returns the full text output. Convenience
	// wrapper over Stream for callers that want the whole blob.
	Result() (string, error)
}
