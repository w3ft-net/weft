//go:build !defaultshape

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

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
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
	// reply, when set, answers each query: the lines to send, or an error.
	reply func(*pb.CoordinatorExecuteRequest) ([]string, error)
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
	lines := []string{"ok"}
	if f.reply != nil {
		var err error
		if lines, err = f.reply(req); err != nil {
			return err
		}
	}
	resp := &pb.CoordinatorExecuteResponse{}
	for _, l := range lines {
		resp.Lines = append(resp.Lines, &pb.Line{Raw: l})
	}
	return stream.Send(resp)
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
	cust, err := s.FindOrCreateOrg(custName)
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	// ActiveSubscription gate in handleShellExec would otherwise 402 us
	// before we reach the coordinator call.
	s.SeedDevSubscription(cust.ID)

	user, err := s.CreateUser(fmt.Sprintf("shellauth-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// The user needs an active membership in the customer for
	// BuildSession (and the active-subscription gate in handleShellExec)
	// to resolve to the seeded subscription.
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
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

	body, _ := json.Marshal(map[string]any{"pipeline": "cat vimgym.log", "follow": false, "range": "1d.."})
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

// shellExecFixture builds the JWT-authenticated test rig used by every
// /api/shell/exec test: a fakeCoord behind a real gRPC server wired into the
// API server's coordClient, plus a valid bearer token. Returned fields are
// the http handler, the fakeCoord (read .lastReq), and the auth header.
type shellExecFixture struct {
	t      *testing.T
	srv    *Server
	fake   *fakeCoord
	bearer string
}

func newShellExecFixture(t *testing.T, tag string) *shellExecFixture {
	t.Helper()
	srv, s := testServer(t)

	custName := fmt.Sprintf("%s-%d", tag, os.Getpid())
	cust, err := s.FindOrCreateOrg(custName)
	if err != nil {
		t.Fatal(err)
	}
	s.SeedDevSubscription(cust.ID)

	user, err := s.CreateUser(fmt.Sprintf("%s-%d@example.com", tag, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCoord{
		authSeen: make(chan string, 8),
		lastReq:  make(chan *pb.CoordinatorExecuteRequest, 8),
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

	return &shellExecFixture{t: t, srv: srv, fake: fake, bearer: "Bearer " + jwt}
}

// post fires a single /api/shell/exec request with the given JSON body and
// returns the coordinator request the fake observed.
func (f *shellExecFixture) post(body map[string]any) *pb.CoordinatorExecuteRequest {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(raw))
	req.Header.Set("Authorization", f.bearer)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	select {
	case got := <-f.fake.lastReq:
		return got
	case <-time.After(2 * time.Second):
		f.t.Fatalf("coordinator Execute was never called (status=%d body=%s)", w.Code, w.Body.String())
		return nil
	}
}

// absDelta is helper for time-near assertions in nanos.
func absDelta(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestShellExec_ResolvesRangeSpec verifies that the API server resolves a
// relative range spec on every request against its own clock. This is the
// fix for the "12-hour-old pane shows stale logs" bug: the client must NOT
// pre-resolve the window — it ships the raw spec and the server anchors it.
func TestShellExec_ResolvesRangeSpec(t *testing.T) {
	f := newShellExecFixture(t, "rangespec")

	beforeNs := time.Now().UnixNano()
	got := f.post(map[string]any{
		"pipeline": "cat vimgym.log",
		"range":    "1d..",
	})
	afterNs := time.Now().UnixNano()

	if got.TimeEndNs < beforeNs || got.TimeEndNs > afterNs {
		t.Errorf("TimeEndNs not anchored to server now: got=%d window=[%d,%d]",
			got.TimeEndNs, beforeNs, afterNs)
	}
	wantStartMin := beforeNs - int64(24*time.Hour)
	wantStartMax := afterNs - int64(24*time.Hour)
	if got.TimeStartNs < wantStartMin || got.TimeStartNs > wantStartMax {
		t.Errorf("TimeStartNs not anchored to now-24h: got=%d wantRange=[%d,%d]",
			got.TimeStartNs, wantStartMin, wantStartMax)
	}
}

// TestShellExec_ResolvesPerRequest fires two requests with the same "1d.."
// spec a measurable delay apart and asserts the resolved start advances. This
// is the bug repro at the server boundary: today the API would scan with
// the same TimeStartNs every time because there's no spec-driven resolution.
func TestShellExec_ResolvesPerRequest(t *testing.T) {
	f := newShellExecFixture(t, "perreq")

	first := f.post(map[string]any{"pipeline": "cat vimgym.log", "range": "1d.."})
	time.Sleep(30 * time.Millisecond)
	second := f.post(map[string]any{"pipeline": "cat vimgym.log", "range": "1d.."})

	if second.TimeStartNs <= first.TimeStartNs {
		t.Errorf("range did not advance between requests: first=%d second=%d (delta=%d)",
			first.TimeStartNs, second.TimeStartNs, second.TimeStartNs-first.TimeStartNs)
	}
	if second.TimeEndNs <= first.TimeEndNs {
		t.Errorf("range end did not advance between requests: first=%d second=%d",
			first.TimeEndNs, second.TimeEndNs)
	}
}

// TestShellExec_RejectsMissingRange asserts the server refuses requests that
// don't carry a Range spec. There's no implicit default: defaults are the
// client's job (CLI/WASM each seed "1d..").
func TestShellExec_RejectsMissingRange(t *testing.T) {
	f := newShellExecFixture(t, "norange")
	raw, _ := json.Marshal(map[string]any{"pipeline": "cat vimgym.log"})
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(raw))
	req.Header.Set("Authorization", f.bearer)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestShellExec_RejectsOldClient pins the version-gate: a request whose
// X-Weft-Client declares a version below MIN_WEFT_CLI_VERSION must be
// answered with HTTP 426 (Upgrade Required) before reaching the
// coordinator. Without this we ship broken old binaries to a confused user
// instead of a clear "weft update" prompt.
func TestShellExec_RejectsOldClient(t *testing.T) {
	t.Setenv("MIN_WEFT_CLI_VERSION", "0.3.0")
	f := newShellExecFixture(t, "oldclient")

	body, _ := json.Marshal(map[string]any{
		"pipeline": "cat vimgym.log",
		"range":    "1d..",
	})
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", f.bearer)
	req.Header.Set("X-Weft-Client", "weft/0.0.1")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)

	if w.Code != 426 {
		t.Fatalf("expected 426, got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not JSON: %v body=%s", err, w.Body.String())
	}
	if resp["error"] != "client too old" {
		t.Errorf("body.error = %v, want 'client too old'", resp["error"])
	}
	if resp["required"] != "0.3.0" {
		t.Errorf("body.required = %v, want 0.3.0", resp["required"])
	}
}

// TestShellExec_AcceptsCurrentClient verifies the gate doesn't false-fire
// when the client is at or above the minimum.
func TestShellExec_AcceptsCurrentClient(t *testing.T) {
	t.Setenv("MIN_WEFT_CLI_VERSION", "0.3.0")
	f := newShellExecFixture(t, "curclient")

	body, _ := json.Marshal(map[string]any{
		"pipeline": "cat vimgym.log",
		"range":    "1d..",
	})
	req := httptest.NewRequest("POST", "/api/shell/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", f.bearer)
	req.Header.Set("X-Weft-Client", "weft/0.3.0")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)

	if w.Code == 426 {
		t.Fatalf("current client got 426: %s", w.Body.String())
	}
	select {
	case <-f.fake.lastReq:
	case <-time.After(2 * time.Second):
		t.Fatalf("coordinator never called for current client (status=%d body=%s)", w.Code, w.Body.String())
	}
}

// TestShellExec_AbsoluteRangeSpec sends a fully-qualified ISO range and
// asserts the resolved nanos match exactly.
func TestShellExec_AbsoluteRangeSpec(t *testing.T) {
	f := newShellExecFixture(t, "absrange")
	got := f.post(map[string]any{
		"pipeline": "cat vimgym.log",
		"range":    "2026-04-01..2026-04-12",
	})
	wantStart, _ := time.Parse("2006-01-02", "2026-04-01")
	wantEnd, _ := time.Parse("2006-01-02", "2026-04-12")
	if got.TimeStartNs != wantStart.UnixNano() {
		t.Errorf("TimeStartNs = %d, want %d", got.TimeStartNs, wantStart.UnixNano())
	}
	if got.TimeEndNs != wantEnd.UnixNano() {
		t.Errorf("TimeEndNs = %d, want %d", got.TimeEndNs, wantEnd.UnixNano())
	}
}
