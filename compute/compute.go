package compute

import (
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
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
type ComputePool interface {
	// NewJob creates a job that will receive rows from shardCount
	// shard streams. mergeKey controls cross-shard ordering: empty
	// concatenates in arrival order (legacy), "line_num"/"raw"
	// (optionally " DESC") triggers a k-way merge keyed on that
	// column. See docs/plans/direct-to-compute.md §4.
	//
	// limit is the global cap on the merged stream applied BEFORE
	// stages run; 0 means no cap. Mirrors planner.QueryPlan.Limit
	// (the user-typed head/tail count). Without it, per-shard
	// LIMIT N gets multiplied by shard count when downstream stages
	// see the union — `tail -n N | wc -l` would return N×num_shards.
	NewJob(stages []string, shardCount int, mergeKey string, limit int) ComputeJob
}

// ComputeJob receives rows from shards and produces a text result once all
// shards have sent their sentinel. It implements shard.RowSink so shards
// can stream directly to it.
type ComputeJob interface {
	shard.RowSink
	// Send delivers a row or sentinel from a shard. Safe for concurrent use.
	Send(msg ComputeMessage)
	// Result blocks until all sentinels are received and the pipeline has
	// finished executing. Returns the text output.
	Result() (string, error)
}
