package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/store"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// fakeCoord stands in for the coordinator's Execute RPC. It records the
// incoming "authorization" metadata so the test can assert the api server
// forwarded the caller's bearer token. If the metadata is missing it
// returns the same Unauthenticated error the real coordinator would, so a
// failure mirrors the prod symptom exactly.
type fakeCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	authSeen chan string
	lastReq  chan *pb.CoordinatorExecuteRequest
}

func (f *fakeCoord) Execute(req *pb.CoordinatorExecuteRequest, stream grpc.ServerStreamingServer[pb.CoordinatorExecuteResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	vals := md.Get("authorization")
	var seen string
	if len(vals) > 0 {
		seen = vals[0]
	}
	select {
	case f.authSeen <- seen:
	default:
	}
	if f.lastReq != nil {
		select {
		case f.lastReq <- req:
		default:
		}
	}
	if seen == "" {
		return status.Errorf(codes.Unauthenticated, "missing authorization metadata")
	}
	return stream.Send(&pb.CoordinatorExecuteResponse{Lines: []*pb.Line{{Raw: "ok"}}})
}

// TestShellExec_ForwardsBearerToCoordinator reproduces the prod symptom
//
//	rpc error: code = Unauthenticated desc = missing authorization metadata
//
// when cat/tail is run through the shell. The api handler was constructing
// the outgoing gRPC context from r.Context() directly instead of via
// s.coordCtx(r), so the caller's JWT never made it onto the wire.
func TestShellExec_ForwardsBearerToCoordinator(t *testing.T) {
	srv, s := testServer(t)

	custName := fmt.Sprintf("shellauth-%d", os.Getpid())
	cust, err := s.FindOrCreateCustomer(custName)
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	// ActiveSubscription gate in handleShellExec would otherwise 402 us
	// before we reach the coordinator call.
	s.SeedDevSubscription(custName)

	user, err := s.CreateUser(fmt.Sprintf("shellauth-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// The user needs an active membership in the customer for
	// BuildSession (and the active-subscription gate in handleShellExec)
	// to resolve to the seeded subscription.
	s.CreateTeamMember(&store.TeamMember{
		ID: "tm-shellauth", CustomerID: cust.ID, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeCoord{authSeen: make(chan string, 1)}
	grpcSrv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(grpcSrv, fake)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake coord: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	srv.coordClient = pb.NewWeftCoordinatorClient(conn)

	body, _ := json.Marshal(map[string]any{"pipeline": "cat vimgym.log", "follow": false})
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Ensure we didn't short-circuit on subscription/body/auth-middleware
	// checks — those would never hit the coordinator.
	var got string
	select {
	case got = <-fake.authSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("coordinator Execute was never called (status=%d body=%s)", w.Code, w.Body.String())
	}

	if got == "" {
		t.Fatalf("api forwarded no authorization metadata to the coordinator; this is the bug — /api/shell/exec built its outgoing gRPC context from r.Context() instead of s.coordCtx(r)")
	}
	if !strings.Contains(got, jwt) {
		t.Errorf("forwarded authorization %q does not carry the caller's JWT", got)
	}

	// And the SSE stream should not contain an error event.
	if strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Errorf("unexpected SSE error in stream:\n%s", w.Body.String())
	}
}

// TestShellExec_ForwardsTimeRange verifies that when the client sends
// time_start_ns / time_end_ns in the request body, those values reach
// the coordinator's Execute RPC. Without this, the user's selected
// range (e.g. "1d") has no effect — every query scans all shards.
func TestShellExec_ForwardsTimeRange(t *testing.T) {
	srv, s := testServer(t)

	custName := fmt.Sprintf("timerange-%d", os.Getpid())
	cust, err := s.FindOrCreateCustomer(custName)
	if err != nil {
		t.Fatal(err)
	}
	s.SeedDevSubscription(custName)

	user, err := s.CreateUser(fmt.Sprintf("timerange-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	s.CreateTeamMember(&store.TeamMember{
		ID: "tm-timerange", CustomerID: cust.ID, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCoord{
		authSeen: make(chan string, 1),
		lastReq:  make(chan *pb.CoordinatorExecuteRequest, 1),
	}
	grpcSrv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(grpcSrv, fake)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	srv.coordClient = pb.NewWeftCoordinatorClient(conn)

	wantStart := time.Now().Add(-24 * time.Hour)
	wantEnd := time.Now()

	body, _ := json.Marshal(map[string]any{
		"pipeline":      "cat vimgym.log",
		"time_start_ns": wantStart.UnixNano(),
		"time_end_ns":   wantEnd.UnixNano(),
	})
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var got *pb.CoordinatorExecuteRequest
	select {
	case got = <-fake.lastReq:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator Execute was never called")
	}

	if got.TimeStartNs == 0 || got.TimeEndNs == 0 {
		t.Fatalf("time range not forwarded: TimeStartNs=%d TimeEndNs=%d (both should be non-zero)",
			got.TimeStartNs, got.TimeEndNs)
	}
	if got.TimeStartNs != wantStart.UnixNano() {
		t.Errorf("TimeStartNs = %d, want %d", got.TimeStartNs, wantStart.UnixNano())
	}
	if got.TimeEndNs != wantEnd.UnixNano() {
		t.Errorf("TimeEndNs = %d, want %d", got.TimeEndNs, wantEnd.UnixNano())
	}
}
