package service

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestIngestBatch_SplitsMultiLineEvents: a multi-line text event reaches
// the shard buffer and live tail as the same one-row-per-line rows, a JSON
// row stays whole, and CRLF is normalized away.
func TestIngestBatch_SplitsMultiLineEvents(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()
	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{
		Compute: &compute.Engine{}, Store: tx, Tail: coordinator.NewTailBroker()})
	srv := &Server{Coord: coord, Store: tx}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ch := coord.TailFanout.Subscribe(ctx, &coordinator.RowMatcher{Namespace: "lines", Customer: int64(cust.ID)})

	json := "{\n \"a\": 1\n}"
	srv.ingestBatch([]*pb.LogRow{
		{Namespace: "lines", Service: "api", CustomerId: int64(cust.ID), Raw: "Traceback:\r\n  File x\r\nValueError\r\n", TsUnixNs: 5},
		{Namespace: "lines", Service: "api", CustomerId: int64(cust.ID), Raw: json, PayloadJson: []byte(`{"a":1}`), TsUnixNs: 6},
	})

	want := []string{"Traceback:", "  File x", "ValueError", json}
	buffered := coord.Ingest.Drain(500)
	if len(buffered) != len(want) {
		t.Fatalf("buffer got %d rows, want %d", len(buffered), len(want))
	}
	for i, w := range want {
		if buffered[i].Raw != w {
			t.Fatalf("buffer row %d = %q, want %q", i, buffered[i].Raw, w)
		}
		select {
		case r := <-ch:
			if r.Raw != w || r.EventId != buffered[i].EventId || r.LineNo != buffered[i].LineNo {
				t.Fatalf("tail row %d = %q %q/%d, buffer has %q %q/%d", i, r.Raw, r.EventId, r.LineNo, buffered[i].Raw, buffered[i].EventId, buffered[i].LineNo)
			}
		case <-time.After(time.Second):
			t.Fatalf("tail never delivered row %d", i)
		}
	}
	if id := buffered[0].EventId; id == "" || buffered[1].EventId != id || buffered[2].EventId != id || buffered[1].LineNo != 1 || buffered[2].LineNo != 2 {
		t.Fatalf("event grouping wrong: %+v", buffered[:3])
	}
	if buffered[3].EventId != "" {
		t.Fatalf("JSON row got event_id %q", buffered[3].EventId)
	}
}
