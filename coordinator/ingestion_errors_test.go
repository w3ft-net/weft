package coordinator

import (
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/ingestfail"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

type captureFailures struct {
	mu  sync.Mutex
	got []ingestfail.Failure
}

func (c *captureFailures) Record(f ingestfail.Failure) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, f)
	return nil
}

func (c *captureFailures) AffectedServices(shape.Added, time.Time) ([]string, error) { return nil, nil }

// A drained batch that fails is recorded as one failure of its owner, naming
// each service in it once.
func TestIngestionErrorRecorder_RecordsTheBatchAsOneFailureOfItsOwner(t *testing.T) {
	st := &captureFailures{}
	rec := NewIngestionErrorRecorder(st)
	var owner shape.Added
	rec.Record("shard-3", "disk full", owner, []*pb.LogRow{
		{Namespace: "ops", Service: "web", Kind: "main"},
		{Namespace: "ops", Service: "web", Kind: "main"},
		{Namespace: "ops", Service: "api", Kind: "main"},
	}, "dlq/batch-9")

	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.got)
		st.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d failures, want 1", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f := st.got[0]
	if f.Owner != owner || f.ShardID != "shard-3" || f.Message != "disk full" || f.Rows != 3 || f.DLQPath != "dlq/batch-9" {
		t.Fatalf("failure = %+v", f)
	}
	if len(f.Services) != 2 {
		t.Fatalf("services = %v, want each of the two once", f.Services)
	}
}

func TestIngestionErrorRecorder_NilStoreRecordsNothing(t *testing.T) {
	if NewIngestionErrorRecorder(nil) != nil {
		t.Fatal("a recorder with no store should be nil")
	}
	var rec *IngestionErrorRecorder
	rec.Record("s", "x", shape.Added{}, nil, "") // must not panic
}
