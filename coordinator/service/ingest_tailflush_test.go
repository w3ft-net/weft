package service

import (
	"context"
	"github.com/graybeardcorp/weft/shape"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestIngestStream_FlushesImmediatelyForActiveTailSubscriber pins the
// known-issues.md lag: a row belonging to a topic with an active `tail
// -f` subscriber should reach that subscriber right away, not sit in
// IngestStream's buffer until the 100-row threshold or the 2-second
// ticker fires (docs/known-issues.md, "`tail -f` can lag up to ~2s under
// sparse ingest").
//
// The stream here sends exactly one row and then blocks (models a
// sparse producer that won't hit the 100-row threshold), so the only
// way the subscriber sees the row promptly is a flush triggered by its
// own interest in the topic.
func TestIngestStream_FlushesImmediatelyForActiveTailSubscriber(t *testing.T) {
	tx, cleanup := testStore(t)
	defer cleanup()

	cust, err := tx.FindOrCreateOrg("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	tx.SeedDevSubscription(cust.ID)
	ns, _ := tx.FindOrCreateNamespace(cust.ID, "flushtest")
	rawTok, _, err := tx.CreateAgentToken(cust.ID, ns.ID, "flush-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{},
		Store: tx,
		Tail:  coordinator.NewTailBroker()})
	srv := &Server{Coord: coord, Store: tx}

	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	_, ch := coord.TailFanout.Subscribe(subCtx, &coordinator.RowMatcher{Coords: shape.Set{{Customer: int64(cust.ID), Namespace: shape.Value("flushtest")}}})

	streamCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))

	st := &abandonedStream{
		ctx:   streamCtx,
		rows:  []*pb.LogRow{{Service: "api", Raw: "sparse row"}},
		block: make(chan struct{}),
	}
	defer close(st.block)

	go func() { _ = srv.IngestStream(st) }()

	select {
	case row := <-ch:
		if row.Raw != "sparse row" {
			t.Fatalf("got row %q, want %q", row.Raw, "sparse row")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("subscriber did not receive the row within 300ms — " +
			"IngestStream is waiting on the 100-row/2s batch instead of " +
			"flushing immediately for a topic with an active tail subscriber")
	}
}
