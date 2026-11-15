// Package dlqstoretest is the conformance suite for dlq.Store. Every store
// passes it: the open build's SQLite one, and the hosted build's S3 one over a
// fake bucket.
package dlqstoretest

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/w3ft-net/weft/backends/dlq"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

var ctx = context.Background()

// Run runs the suite; newStore returns an empty store for each case.
func Run(t *testing.T, newStore func(t *testing.T) dlq.Store) {
	for _, c := range []struct {
		name string
		fn   func(*testing.T, dlq.Store)
	}{
		{"ReplayRestoresEveryRowExactly", restoresExactly},
		{"ReplayEmptiesTheStore", replayEmpties},
		{"ABatchStaysUntilTheSinkReturns", staysUntilSinkReturns},
		{"AnEmptyBatchIsKeptAsNothing", emptyBatch},
		{"EachBatchKeepsItsOwnersColumns", ownersKeepTheirColumns},
		{"ReplayOfAnEmptyStoreDeliversNothing", replayEmptyStore},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore(t)) })
	}
}

// FullRow sets fields a lossy format would drop (the trace group, dimensions,
// the event id), so a lossy round trip shows up as a diff.
func FullRow(i int) *pb.LogRow {
	return &pb.LogRow{
		Raw: "full row", Namespace: "weft", Service: "coordinator", Kind: "main",
		Level: "info", TsUnixNs: 1_700_000_000_000_000_000 + int64(i), Instance: "i-123",
		PayloadJson: []byte(`{"k":"v"}`), Deployment: "prod", Env: "prod", Version: "v1.2.3",
		Region: "us-east-2", Node: "n1", TraceId: "b493e2ab050433a5a0ce2a52db32116e",
		SpanName: "upgrade.pause", SpanId: "s1", EventId: "evt-1", LineNo: int32(i),
	}
}

func write(t *testing.T, s dlq.Store, owner int, rows ...*pb.LogRow) string {
	t.Helper()
	ref, err := s.Write(ctx, shapetest.Added(int64(owner)), rows)
	if err != nil || ref == "" {
		t.Fatalf("Write = %q, %v", ref, err)
	}
	return ref
}

func replay(t *testing.T, s dlq.Store) []dlq.Entry {
	t.Helper()
	var got []dlq.Entry
	n, err := s.Replay(ctx, func(e []dlq.Entry) { got = append(got, e...) })
	if err != nil || n != len(got) {
		t.Fatalf("Replay = %d, %v; the sink saw %d", n, err, len(got))
	}
	return got
}

func restoresExactly(t *testing.T, s dlq.Store) {
	want := []*pb.LogRow{FullRow(1), FullRow(2), FullRow(3)}
	write(t, s, 1, want...)
	got := replay(t, s)
	if len(got) != len(want) {
		t.Fatalf("replayed %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(want[i], got[i].LogRow) || got[i].Added != shapetest.Added(1) {
			t.Errorf("row %d changed in the store:\n want %v %+v\n  got %v %+v", i, want[i], shapetest.Added(1), got[i].LogRow, got[i].Added)
		}
	}
}

func replayEmpties(t *testing.T, s dlq.Store) {
	write(t, s, 1, FullRow(1))
	if got := replay(t, s); len(got) != 1 {
		t.Fatalf("first replay delivered %d rows, want 1", len(got))
	}
	if got := replay(t, s); len(got) != 0 {
		t.Errorf("%d rows remained after their delivery", len(got))
	}
}

// A sink that dies mid-delivery (a crash) leaves its batch to be delivered
// again: a batch is removed only after the sink has taken its rows.
func staysUntilSinkReturns(t *testing.T, s dlq.Store) {
	write(t, s, 1, FullRow(1))
	func() {
		defer func() { recover() }()
		s.Replay(ctx, func([]dlq.Entry) { panic("the process died") })
	}()
	if got := replay(t, s); len(got) != 1 {
		t.Errorf("the batch was lost when its sink died: %d rows replayed, want 1", len(got))
	}
}

func emptyBatch(t *testing.T, s dlq.Store) {
	ref, err := s.Write(ctx, shapetest.Added(1), nil)
	if err != nil || ref != "" {
		t.Errorf("Write of no rows = %q, %v; want \"\", nil", ref, err)
	}
	if got := replay(t, s); len(got) != 0 {
		t.Errorf("an empty batch left %d rows", len(got))
	}
}

func ownersKeepTheirColumns(t *testing.T, s dlq.Store) {
	for owner := 1; owner <= 3; owner++ {
		write(t, s, owner, &pb.LogRow{Namespace: "weft", Service: "coordinator", Raw: fmt.Sprintf("owner-%d", owner)})
	}
	got := replay(t, s)
	if len(got) != 3 {
		t.Fatalf("replayed %d rows, want 3", len(got))
	}
	for _, e := range got {
		var owner int
		fmt.Sscanf(e.Raw, "owner-%d", &owner)
		if e.Added != shapetest.Added(int64(owner)) {
			t.Errorf("%s replayed with %+v, want owner %d's columns", e.Raw, e.Added, owner)
		}
	}
}

func replayEmptyStore(t *testing.T, s dlq.Store) {
	if got := replay(t, s); len(got) != 0 {
		t.Errorf("an empty store replayed %d rows", len(got))
	}
}
