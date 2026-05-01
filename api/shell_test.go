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
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// fakeCoord stands in for the coordinator's Query RPC. It records the
// incoming "authorization" metadata so the test can assert the api server
// forwarded the caller's bearer token. If the metadata is missing it
// returns the same Unauthenticated error the real coordinator would, so a
// failure mirrors the prod symptom exactly.
type fakeCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	authSeen chan string
}

func (f *fakeCoord) Query(_ *pb.CoordinatorExecuteRequest, stream grpc.ServerStreamingServer[pb.CoordinatorExecuteResponse]) error {
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

	user, err := s.CreateUser(fmt.Sprintf("shellauth-%d@example.com", os.Getpid()), cust.ID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Generate() reads CustomerName off the User struct; CreateUser
	// leaves it blank, so set it so the JWT claim matches the
	// subscription we just seeded.
	user.CustomerName = custName
	jwt, err := auth.Generate(user, "trial", "test-secret")
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
		t.Fatalf("coordinator Query was never called (status=%d body=%s)", w.Code, w.Body.String())
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
