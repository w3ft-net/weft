package hostedapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// followFakeCoord simulates a real `tail -f` stream: it sends one line
// then blocks until the client cancels, exactly like the real
// coordinator's follow mode (which never naturally reaches io.EOF).
type followFakeCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	firstLineSent chan struct{}
}

func (f *followFakeCoord) Execute(req *pb.CoordinatorExecuteRequest, stream grpc.ServerStreamingServer[pb.CoordinatorExecuteResponse]) error {
	if err := stream.Send(&pb.CoordinatorExecuteResponse{Lines: []*pb.Line{{Raw: "line one"}}}); err != nil {
		return err
	}
	select {
	case f.firstLineSent <- struct{}{}:
	default:
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// TestShellExec_FollowCancelAdvancesLesson is the regression test for
// api/shell.go's handleShellExec ctx.Done() fix: a tail -f stream
// never naturally EOFs, so lesson advancement has to be checked when
// the client cancels (the only way anyone actually stops a live
// tail), not just on the io.EOF branch. Without the fix, a user on
// the tour's shell-tail-f lesson could never advance past it.
func TestShellExec_FollowCancelAdvancesLesson(t *testing.T) {
	srv, s := testServer(t)

	loomCust, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom customer: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loomCust.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}
	s.SeedDevSubscription(loomCust.ID)

	user, err := s.CreateUser(fmt.Sprintf("followadvance-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.AddTeamMemberWithEmail(loomCust.ID, user.ID, user.Email, store.RoleOwner); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	// Put the user on the lesson right before shell-tail-f advances to.
	if ok, err := s.AdvanceOnboardingStep(user.ID, "welcome", "shell-tail-f"); err != nil || !ok {
		t.Fatalf("seed onboarding step: ok=%v err=%v", ok, err)
	}
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &followFakeCoord{firstLineSent: make(chan struct{}, 1)}
	grpcSrv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(grpcSrv, fake)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake coord: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	srv.SetCoordClient(pb.NewWeftCoordinatorClient(conn))

	body, _ := json.Marshal(map[string]any{
		"pipeline": "tail -f /log/loom/prod/api/main.log",
		"follow":   true,
		"range":    "1d..",
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-fake.firstLineSent:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator never received the follow request / sent a line")
	}

	// Simulate the client stopping the live tail — the only way anyone
	// actually ends a tail -f in practice.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never returned after ctx cancellation")
	}

	updated, err := s.GetUserByID(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if updated.OnboardingStep != "shell-cd" {
		t.Errorf("OnboardingStep after cancelling tail -f = %q, want %q (the lesson after shell-tail-f)",
			updated.OnboardingStep, "shell-cd")
	}
}
