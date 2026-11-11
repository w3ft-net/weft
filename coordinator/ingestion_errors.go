package coordinator

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"strconv"
	"strings"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// IngestionErrorRecorder asynchronously records ingest failures to postgres.
type IngestionErrorRecorder struct {
	store *store.Store
	ch    chan ingestionError
}

type ingestionError struct {
	ShardID   string
	ErrorMsg  string
	RowCount  int
	Apps      []string
	Customers []string
	DLQPath   string
}

// NewIngestionErrorRecorder creates a recorder with a buffered channel.
// Returns nil if store is nil.
func NewIngestionErrorRecorder(s *store.Store) *IngestionErrorRecorder {
	if s == nil {
		return nil
	}
	r := &IngestionErrorRecorder{
		store: s,
		ch:    make(chan ingestionError, 100),
	}
	go r.flush()
	return r
}

// Record queues an ingestion error for async persistence.
func (r *IngestionErrorRecorder) Record(shardID, errMsg string, added shape.Added, rows []*pb.LogRow, dlqPath string) {
	if r == nil {
		return
	}

	apps := distinctStrings(rows, func(r *pb.LogRow) string { return shard.JoinApp(r.Namespace, r.Service, r.Kind) })
	customers := []string{strconv.FormatInt(added.Customer, 10)}

	select {
	case r.ch <- ingestionError{
		ShardID:   shardID,
		ErrorMsg:  errMsg,
		RowCount:  len(rows),
		Apps:      apps,
		Customers: customers,
		DLQPath:   dlqPath,
	}:
	default:
		weftsdk.Errorf(context.Background(), "ingestion-errors: channel full, dropping error record for %s", shardID)
	}
}

func (r *IngestionErrorRecorder) flush() {
	for ie := range r.ch {
		if err := r.store.DB().Exec(`
			INSERT INTO ingestion_errors (shard_id, error_msg, row_count, apps, customers, dlq_path)
			VALUES (?, ?, ?, ?, ?, ?)`,
			ie.ShardID, ie.ErrorMsg, ie.RowCount,
			"{"+strings.Join(ie.Apps, ",")+"}",
			"{"+strings.Join(ie.Customers, ",")+"}",
			ie.DLQPath,
		).Error; err != nil {
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
