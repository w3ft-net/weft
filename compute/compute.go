package compute

import (
	"context"
	"sync"

	"github.com/graybeardcorp/weft/internal/weftlog"
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

// Executor is what the coordinator dispatches every query through.
// Production uses Client (gRPC to the remote compute worker, which
// runs an Engine); tests use Engine directly, in-process.
type Executor interface {
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
type ResolveShardFunc func(ctx context.Context, d *pb.ShardDescriptor) (shard.Shard, error)

// FanOutToShards turns each descriptor into a handle (via resolve),
// runs QueryToSink concurrently, and waits. The resolver owns
// connection caching and rebind invalidation; FanOut is intentionally
// dumb about lifecycle — it does not Close the handle on return,
// because the engine's cache may keep it for the next query.
func FanOutToShards(ctx context.Context, req *pb.ExecuteQueryRequest, job shard.RowSink, resolve ResolveShardFunc) {
	args := make([]any, len(req.Args))
	for i, a := range req.Args {
		args[i] = a
	}
	var wg sync.WaitGroup
	wg.Add(len(req.Shards))
	for _, desc := range req.Shards {
		go func(d *pb.ShardDescriptor) {
			defer wg.Done()
			sh, err := resolve(ctx, d)
			if err != nil {
				weftlog.Infof(ctx, "execute-query: resolve shard %s: %v", d.ShardId, err)
				job.SendSentinel(d.ShardId)
				return
			}
			sh.QueryToSink(ctx, req.Sql, args, job)
		}(desc)
	}
	wg.Wait()
}
