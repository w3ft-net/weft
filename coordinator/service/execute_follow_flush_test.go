package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// flushCaptureStream is notice_wire_test.go's captureStream, made safe
// for concurrent Send (Execute runs in its own goroutine here while the
// test asserts on arrival) and signaling each Send on a channel so the
// test can assert delivery latency directly instead of polling.
type flushCaptureStream struct {
	grpc.ServerStream
	ctx context.Context

	mu        sync.Mutex
	responses []*pb.CoordinatorExecuteResponse
	sent      chan *pb.CoordinatorExecuteResponse
}

func newFlushCaptureStream(ctx context.Context) *flushCaptureStream {
	return &flushCaptureStream{ctx: ctx, sent: make(chan *pb.CoordinatorExecuteResponse, 16)}
}

func (s *flushCaptureStream) Context() context.Context { return s.ctx }
func (s *flushCaptureStream) Send(r *pb.CoordinatorExecuteResponse) error {
	s.mu.Lock()
	s.responses = append(s.responses, r)
	s.mu.Unlock()
	s.sent <- r
	return nil
}
func (s *flushCaptureStream) SetHeader(metadata.MD) error  { return nil }
func (s *flushCaptureStream) SendHeader(metadata.MD) error { return nil }
func (s *flushCaptureStream) SetTrailer(metadata.MD)       {}
func (s *flushCaptureStream) SendMsg(any) error            { return nil }
func (s *flushCaptureStream) RecvMsg(any) error            { return nil }

// TestExecuteRPC_FollowFlushesSingleLineImmediately guards against the
// Execute handler's batchSize=500 response-batching threshold silently
// holding a live tail -f row: a follow session that never accumulates
// 500 rows and never naturally ends must still see each row promptly,
// not only once the batch fills or the stream tears down.
func TestExecuteRPC_FollowFlushesSingleLineImmediately(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()

	cust, _ := st.FindOrCreateOrg("follow-flush")
	st.SeedDevSubscription(cust.ID)
	if _, err := st.FindOrCreateNamespace(cust.ID, "follow-flush"); err != nil {
		t.Fatal(err)
	}
	// A live tail reads only what its caller may, so the caller owns the org.
	user, err := st.CreateVerifiedUser("flush@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{},
		Store: st,
		Tail:  coordinator.NewTailBroker()})
	srv := &Server{Coord: coord, Store: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = auth.WithClaims(ctx, &auth.Claims{UserID: user.ID})
	ctx = auth.WithSession(ctx, &auth.Session{
		UserID:             user.ID,
		Email:              user.Email,
		ActiveOrgID:        cust.ID,
		ActiveCustomerName: cust.Name,
		EmailVerified:      true,
	})

	req := &pb.CoordinatorExecuteRequest{
		Pipeline: "tail follow-flush/svc/main.log",
		Follow:   true,
	}

	stream := newFlushCaptureStream(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Execute(req, stream) }()

	// Give ExecuteFollow's historical phase (empty here — no shard data
	// for this brand-new namespace/service) time to exhaust and
	// subscribe to the broker before broadcasting.
	time.Sleep(200 * time.Millisecond)

	coord.TailFanout.Publish([]coordinator.Row{{LogRow: &pb.LogRow{
		Namespace: "follow-flush", Service: "svc", Kind: "main",
		Raw: "single live row",
	}, Added: shape.Added{Customer: int64(cust.ID)}}})

	select {
	case resp := <-stream.sent:
		found := false
		for _, l := range resp.Lines {
			if l.Raw == "single live row" {
				found = true
			}
		}
		if !found {
			t.Fatalf("first Send did not carry the live row: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the live row to flush — response batching is holding it back")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return after context cancel")
	}
}
