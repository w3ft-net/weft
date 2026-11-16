// Package live is the contract of a live log store, the engine a shard node
// runs: what it must do for the node to ingest into it, query it, archive it and
// report on it. DuckDB is the store the open build ships; see
// docs/plans/pluggable-log-storage.md.
//
// A store is the node-side engine. The coordinator-side client of a node is
// shard.Shard, which this package does not define. The package imports nothing
// that links a database, so a store built on another engine does not link
// DuckDB.
package live

import (
	"context"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// Store is a live log store, one shard's worth of rows.
type Store interface {
	// Layout is the partition columns every row carries.
	Layout() shape.Layout

	// QueryContextTable runs the planner's SQL, in the store's dialect, and
	// returns its rows. table, when set, names the table every row the query
	// reads is in (planner.QueryPlan.Table); a store that keeps one table or
	// reads whatever the SQL names ignores it.
	QueryContextTable(ctx context.Context, table, sql string, args ...any) ([]*pb.ResultRow, error)

	// Ingest and IngestBatch append rows. rec, which may be nil, hears the
	// outcome (see IngestRecorder).
	Ingest(ctx context.Context, rows []*pb.LogRow, rec IngestRecorder) error
	IngestBatch(ctx context.Context, b Batch, rec IngestRecorder) error

	// Archive exports the store's rows under s3Prefix and returns the path
	// written and each partition's row count and time span.
	Archive(ctx context.Context, s3Prefix string) (path string, stats []PartitionStat, err error)

	// Checkpoint makes what has been written durable on the data volume, ahead
	// of the volume being detached.
	Checkpoint(ctx context.Context) error
	// Close releases the store.
	Close() error

	// RowCount, DiskUsage and QueryStatsSnapshot are what the node reports of
	// the store on its heartbeat. OpenTiming is a line saying how long opening it
	// took, for the log.
	RowCount() int64
	DiskUsage() DiskUsage
	QueryStatsSnapshot() QueryStatsSnapshot
	OpenTiming() string
}

// FieldFactSource is a store that learns which fields of its sources hold only
// numbers, so the planner can offer shards that have said so a narrower query
// (planner.TypedPlan). It is optional: a store without it reports no facts, and
// every query runs the base SQL there.
type FieldFactSource interface {
	// FieldFacts are the facts the coordinator has not yet acknowledged.
	FieldFacts() []FieldFact
	// AckFieldFacts records that the coordinator has every fact up to epoch.
	AckFieldFacts(epoch uint64)
	// FlushFieldFacts saves what the store has learned, ahead of it being
	// closed or detached.
	FlushFieldFacts()
}
