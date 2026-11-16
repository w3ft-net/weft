package live

import (
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// Batch is rows bound for one store that all belong to one partition.
// PartitionValues are the partition's values, positional per the shard's
// layout. nil means the batch names none, and the legacy layout then reads each
// row's customer_id.
type Batch struct {
	Rows            []*pb.LogRow
	PartitionValues []string
}

// IngestRecorder is the surface a store's Ingest uses to report its own
// outcome back to the failure-count / suspension tracker (the provisioner).
// The store formats the error string from
// (count, threshold) so the caller only needs one log.Printf to
// surface the full state transition.
//
// Nil-safe: callers that don't care about routing failover (tests,
// in-process server paths) pass nil and Ingest behaves like the old
// bare-error form.
type IngestRecorder interface {
	// RecordIngestFailure increments the consecutive-failure counter
	// for shardID and returns the new count plus the threshold at
	// which the recorder considers the shard suspended.
	RecordIngestFailure(shardID string) (count, threshold int)
	// ResetIngestFailures clears the counter on a successful ingest.
	ResetIngestFailures(shardID string)
}

// DiskUsage is a store's data-volume footprint. TotalBytes() is what to compare
// against the volume size for the ≤50%-co-located-spill rule.
type DiskUsage struct {
	DataBytes     int64 // size of the main data file (checkpointed data)
	WALBytes      int64 // size of the write-ahead log (writes not yet folded into the main file)
	SpillTmpBytes int64 // total bytes under <db>.tmp (DuckDB's spill directory)
}

// TotalBytes is the combined .duckdb + WAL + spill footprint on the data volume.
func (u DiskUsage) TotalBytes() int64 { return u.DataBytes + u.WALBytes + u.SpillTmpBytes }

// QueryStatsSnapshot is one telemetry interval's query-latency aggregate.
type QueryStatsSnapshot struct {
	Count  int64   // queries executed in the interval
	Errors int64   // of which failed
	Rows   int64   // total rows returned
	MeanMs float64 // mean execution time (ms)
	MaxMs  float64 // slowest single query (ms) — the spill-sensitive tail
}
