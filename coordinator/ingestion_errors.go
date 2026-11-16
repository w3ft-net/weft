package coordinator

import (
	"context"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/ingestfail"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shard"
)

// IngestionErrorRecorder asynchronously records ingest failures, so a failing
// batch never waits on the record of its failure.
type IngestionErrorRecorder struct {
	store ingestfail.Store
	ch    chan ingestfail.Failure
}

// NewIngestionErrorRecorder creates a recorder with a buffered channel.
// Returns nil if store is nil.
func NewIngestionErrorRecorder(s ingestfail.Store) *IngestionErrorRecorder {
	if s == nil {
		return nil
	}
	r := &IngestionErrorRecorder{
		store: s,
		ch:    make(chan ingestfail.Failure, 100),
	}
	go r.flush()
	return r
}

// Record queues an ingestion error for async persistence.
func (r *IngestionErrorRecorder) Record(shardID, errMsg string, added shape.Added, rows []*pb.LogRow, dlqPath string) {
	if r == nil {
		return
	}
	select {
	case r.ch <- ingestfail.Failure{
		Owner:    added,
		ShardID:  shardID,
		Message:  errMsg,
		Rows:     len(rows),
		Services: distinctStrings(rows, func(r *pb.LogRow) string { return shard.JoinApp(r.Namespace, r.Service, r.Kind) }),
		DLQPath:  dlqPath,
	}:
	default:
		weftsdk.Errorf(context.Background(), "ingestion-errors: channel full, dropping error record for %s", shardID)
	}
}

func (r *IngestionErrorRecorder) flush() {
	for f := range r.ch {
		if err := r.store.Record(f); err != nil {
			weftsdk.Errorf(context.Background(), "ingestion-errors: failed to persist: %v", err)
		}
	}
}

func distinctStrings(rows []*pb.LogRow, extract func(*pb.LogRow) string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range rows {
		v := extract(r)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
