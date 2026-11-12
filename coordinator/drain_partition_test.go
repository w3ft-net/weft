package coordinator

import (
	"context"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"slices"
	"sync"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/shard"
)

// recordingShard remembers the batches it was handed.
type recordingShard struct {
	countingShard
	bmu     sync.Mutex
	batches []shard.Batch
}

func (s *recordingShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	s.bmu.Lock()
	s.batches = append(s.batches, b)
	s.bmu.Unlock()
	return s.countingShard.Ingest(ctx, b.Rows, rec)
}

// A batch is one customer's rows, and tells its shard so: the customer is the
// batch's partition value, so the shard need not read it off every row.
func TestIngestBatch_CarriesThePartitionValues(t *testing.T) {
	dlq, _ := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	sh := &recordingShard{countingShard: countingShard{dlqFailingShard: dlqFailingShard{id: "shard-rec"}}}
	seedIngesting(c, sh.id, sh)

	c.ingestBatch(context.Background(), drainBatch{
		key:  drainKey{added: shapetest.Added(42), namespace: "weft", service: "coordinator"},
		sh:   sh,
		rows: dlqTestRows("weft/coordinator", 2),
	})

	if len(sh.batches) != 1 {
		t.Fatalf("shard got %d batches, want 1", len(sh.batches))
	}
	if got, want := sh.batches[0].PartitionValues, shapetest.Partition(42); !slices.Equal(got, want) {
		t.Errorf("PartitionValues = %v, want %v", got, want)
	}
}
