// Package livetest is the conformance suite for live.Store. Every store passes
// it: the open build's DuckDB one, and any store a deployment builds on another
// engine.
package livetest

import (
	"context"
	"sync"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

var ctx = context.Background()

// Spec is the store under test, and what the suite needs to know to drive it.
type Spec struct {
	// New opens an empty store. The suite closes it, once.
	New func(t *testing.T) live.Store
	// SelectAll is a query, in the store's dialect, for every row's raw and ts in
	// ts order: (raw, ts) are the first two columns of any query a store runs.
	SelectAll string
	// PartitionValues are the values of one partition of the store's layout, which
	// every batch is ingested under; nil for a layout with no partition columns.
	PartitionValues []string
}

// RunDriver checks a driver and runs the suite over the stores it opens. spec's
// New is the driver's Open, with the shard ID "conformance" and no path.
func RunDriver(t *testing.T, d live.Driver, spec Spec) {
	t.Run("the driver is named and has a dialect", func(t *testing.T) {
		if d.Name() == "" {
			t.Error("the driver has no name")
		}
		if d.Dialect() == nil || d.Dialect().Name() == "" {
			t.Error("the driver has no dialect, or its dialect has no name")
		}
	})
	spec.New = func(t *testing.T) live.Store {
		s, err := d.Open(live.OpenParams{ID: "conformance"})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if s == nil {
			t.Fatal("open returned no store and no error")
		}
		return s
	}
	Run(t, spec)
}

// recorder counts what a store reports of an ingest.
type recorder struct {
	mu              sync.Mutex
	failures, reset int
}

func (r *recorder) RecordIngestFailure(string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	return r.failures, 3
}

func (r *recorder) ResetIngestFailures(string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reset++
}

func rows(raws ...string) []*pb.LogRow {
	out := make([]*pb.LogRow, len(raws))
	for i, raw := range raws {
		out[i] = &pb.LogRow{Raw: raw, TsUnixNs: int64(1_700_000_000+i) * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}
	}
	return out
}

// Run runs the suite; each case opens its own store.
func Run(t *testing.T, spec Spec) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, live.Store, Spec)
	}{
		{"an empty store has no rows", emptyStore},
		{"ingested rows are counted and read back in ts order", ingestAndRead},
		{"a batch is the same as its rows", batchEqualsRows},
		{"a successful ingest is reported to the recorder, and not as a failure", recorderHearsSuccess},
		{"an ingest with no rows is not an error", emptyIngest},
		{"queries are counted", queriesAreCounted},
		{"a checkpoint keeps what was ingested", checkpointKeepsRows},
		{"a store reports its disk usage", diskUsage},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := spec.New(t)
			// Every case ends with the store closed, once, however it ends.
			defer func() {
				if err := s.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			c.fn(t, s, spec)
		})
	}
}

func ingest(t *testing.T, s live.Store, spec Spec, rec live.IngestRecorder, raws ...string) {
	t.Helper()
	if err := s.IngestBatch(ctx, live.Batch{Rows: rows(raws...), PartitionValues: spec.PartitionValues}, rec); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

func read(t *testing.T, s live.Store, spec Spec) []string {
	t.Helper()
	got, err := s.QueryContextTable(ctx, "", spec.SelectAll)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var raws []string
	for _, r := range got {
		raws = append(raws, r.Raw)
	}
	return raws
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func emptyStore(t *testing.T, s live.Store, spec Spec) {
	if n := s.RowCount(); n != 0 {
		t.Errorf("RowCount = %d", n)
	}
	if got := read(t, s, spec); len(got) != 0 {
		t.Errorf("rows = %v", got)
	}
}

func ingestAndRead(t *testing.T, s live.Store, spec Spec) {
	ingest(t, s, spec, nil, "a", "b", "c")
	if n := s.RowCount(); n != 3 {
		t.Errorf("RowCount = %d, want 3", n)
	}
	if got := read(t, s, spec); !equal(got, []string{"a", "b", "c"}) {
		t.Errorf("rows = %v", got)
	}
	ingest(t, s, spec, nil, "d")
	if n := s.RowCount(); n != 4 {
		t.Errorf("RowCount after a second ingest = %d, want 4", n)
	}
}

func batchEqualsRows(t *testing.T, s live.Store, spec Spec) {
	if spec.PartitionValues != nil {
		t.Skip("Ingest names no partition: only a layout with no partition columns can use it")
	}
	if err := s.Ingest(ctx, rows("x", "y"), nil); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	ingest(t, s, spec, nil, "z")
	if n := s.RowCount(); n != 3 {
		t.Errorf("RowCount = %d, want 3", n)
	}
}

func recorderHearsSuccess(t *testing.T, s live.Store, spec Spec) {
	rec := &recorder{}
	ingest(t, s, spec, rec, "a")
	if rec.failures != 0 {
		t.Errorf("a successful ingest was recorded as %d failures", rec.failures)
	}
	if rec.reset == 0 {
		t.Error("a successful ingest did not clear the failure count")
	}
}

func emptyIngest(t *testing.T, s live.Store, spec Spec) {
	if err := s.IngestBatch(ctx, live.Batch{PartitionValues: spec.PartitionValues}, nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
	if n := s.RowCount(); n != 0 {
		t.Errorf("RowCount = %d", n)
	}
}

func queriesAreCounted(t *testing.T, s live.Store, spec Spec) {
	ingest(t, s, spec, nil, "a")
	before := s.QueryStatsSnapshot().Count
	read(t, s, spec)
	read(t, s, spec)
	if got := s.QueryStatsSnapshot().Count + before; got < 2 {
		t.Errorf("two queries counted as %d", got)
	}
}

func checkpointKeepsRows(t *testing.T, s live.Store, spec Spec) {
	ingest(t, s, spec, nil, "a", "b")
	// As the node does it: ingest has stopped, the store's own writers are
	// flushed, and then it is checkpointed.
	if f, ok := s.(live.FieldFactSource); ok {
		f.FlushFieldFacts()
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if got := read(t, s, spec); !equal(got, []string{"a", "b"}) {
		t.Errorf("rows after a checkpoint = %v", got)
	}
}

func diskUsage(t *testing.T, s live.Store, spec Spec) {
	ingest(t, s, spec, nil, "a")
	if u := s.DiskUsage(); u.TotalBytes() < 0 || u.DataBytes < 0 || u.WALBytes < 0 || u.SpillTmpBytes < 0 {
		t.Errorf("negative usage: %+v", u)
	}
}
