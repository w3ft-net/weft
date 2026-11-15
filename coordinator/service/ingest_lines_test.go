package service

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// TestIngestBatch_SplitsMultiLineEvents: a multi-line text event reaches
// the shard buffer and live tail as the same one-row-per-line rows, a JSON
// row stays whole, and CRLF is normalized away.
func TestIngestBatch_SplitsMultiLineEvents(t *testing.T) {
	env := newSvcEnv(t)

	srv := env.server(coordinator.CoordinatorConfig{Tail: coordinator.NewTailBroker()}, nil)
	coord := srv.Coord

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, ch := coord.TailFanout.Subscribe(ctx, &coordinator.RowMatcher{Coords: env.inNamespace("lines")})

	json := "{\n \"a\": 1\n}"
	srv.ingestBatch(context.Background(), env.owner, []*pb.LogRow{
		{Namespace: "lines", Service: "api", Raw: "Traceback:\r\n  File x\r\nValueError\r\n", TsUnixNs: 5},
		{Namespace: "lines", Service: "api", Raw: json, PayloadJson: []byte(`{"a":1}`), TsUnixNs: 6},
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
		if buffered[i].Added != env.owner {
			t.Fatalf("buffer row %d belongs to %+v, want %+v", i, buffered[i].Added, env.owner)
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

// TestIngestBatch_JSONRawFillsPayload: an SDK record (weft-go Send, a
// heartbeat) reaches the coordinator as a JSON line with no payload, since
// its socket frame carries no fmt=json header and its source has no schema.
// jq reads the payload, so without one every field reads as null. A JSON
// object line becomes its own payload; anything else is left alone.
func TestIngestBatch_JSONRawFillsPayload(t *testing.T) {
	env := newSvcEnv(t)

	srv := env.server(coordinator.CoordinatorConfig{Tail: coordinator.NewTailBroker()}, nil)
	coord := srv.Coord

	record := `{"coordinator_id":"i-1","goroutines":244}`
	srv.ingestBatch(context.Background(), env.owner, []*pb.LogRow{
		{Namespace: "weft", Service: "coordinator", Kind: "runtime", Raw: record, TsUnixNs: 1},
		{Namespace: "weft", Service: "coordinator", Raw: `[1,2]`, TsUnixNs: 2},
		{Namespace: "weft", Service: "coordinator", Raw: `{not json`, TsUnixNs: 3},
		{Namespace: "weft", Service: "coordinator", Raw: `level=info {"a":1}`, PayloadJson: []byte(`{"a":1}`), TsUnixNs: 4},
	})

	want := []string{record, "", "", `{"a":1}`}
	buffered := coord.Ingest.Drain(500)
	if len(buffered) != len(want) {
		t.Fatalf("buffer got %d rows, want %d", len(buffered), len(want))
	}
	for i, w := range want {
		if got := string(buffered[i].PayloadJson); got != w {
			t.Errorf("row %d (%q) payload = %q, want %q", i, buffered[i].Raw, got, w)
		}
	}
}
