package service

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
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
	env := newSvcEnv(t)
	rawTok := env.agentToken("flushtest", "flush-agent")

	srv := env.server(coordinator.CoordinatorConfig{Tail: coordinator.NewTailBroker()}, &Server{})
	coord := srv.Coord

	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	_, ch := coord.TailFanout.Subscribe(subCtx, &coordinator.RowMatcher{Coords: env.inNamespace("flushtest")})

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

// A row off the ingest stream carries its tenant beside it, in the added
// columns. It no longer carries it in tag 26: the coordinator reads no tenant
// there, and no shard or peer coordinator is left that does.
func TestIngestStream_CarriesTheTenantBesideTheRowNotInTag26(t *testing.T) {
	env := newSvcEnv(t)
	rawTok := env.agentToken("tag26test", "tag26-agent")

	srv := env.server(coordinator.CoordinatorConfig{Tail: coordinator.NewTailBroker()}, &Server{})
	coord := srv.Coord

	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	_, ch := coord.TailFanout.Subscribe(subCtx, &coordinator.RowMatcher{Coords: env.inNamespace("tag26test")})

	streamCtx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))
	st := &abandonedStream{
		ctx:   streamCtx,
		rows:  []*pb.LogRow{{Service: "api", Raw: "tenant row"}},
		block: make(chan struct{}),
	}
	defer close(st.block)
	go func() { _ = srv.IngestStream(st) }()

	select {
	case row := <-ch:
		if row.CustomerId != 0 {
			t.Errorf("the row carries tag 26 = %d, want it unset", row.CustomerId)
		}
		if want := env.owner; row.Added != want {
			t.Errorf("the row's added columns are %+v, want %+v", row.Added, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the row never reached the subscriber")
	}
}
