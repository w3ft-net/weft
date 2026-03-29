package compute

import "github.com/kevlarcon/weft/shard"

// ComputeMessage is sent from shards to a compute job. Either Row is set
// (a data row) or Sentinel is true (indicating the shard has finished).
type ComputeMessage struct {
	Row      *shard.LogRow
	ShardID  string
	Sentinel bool
}

// ComputePool creates compute jobs. Implementations:
//   - LocalComputePool: in-process execution via warp's bash interpreter
//   - (future) GRPCComputePool: fans out to remote compute workers
type ComputePool interface {
	NewJob(stages []string, shardCount int) ComputeJob
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
