package shard

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// retryServer is a controllable ShardQueryServer for retry tests. It
// fails the first `failBeforeOK` Query calls with the given gRPC code,
// then succeeds with one row and EOF.
type retryServer struct {
	pb.UnimplementedShardQueryServer
	failBeforeOK int32
	failCode     codes.Code
	failMsg      string
	calls        atomic.Int32
}

func (s *retryServer) Query(_ *pb.QueryRequest, stream grpc.ServerStreamingServer[pb.ResultRow]) error {
	n := s.calls.Add(1)
	if n <= s.failBeforeOK {
		return status.Error(s.failCode, s.failMsg)
	}
	return stream.Send(&pb.ResultRow{Raw: "after-retry", TsUnixNs: coldT0.UnixNano()})
}

func startRetryServer(t *testing.T, srv *retryServer) (addr string, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	return lis.Addr().String(), func() { gs.GracefulStop() }
}

// TestGRPCShard_Query_RetriesOnCanceled is the headline regression
// test for the rebind-mid-flight race documented in
// docs/plans/shard-grpc-resilience.md. A `Canceled` error from the
// shard side should trigger a retry; the second attempt sees the row
// and returns success.
func TestGRPCShard_Query_RetriesOnCanceled(t *testing.T) {
	srv := &retryServer{
		failBeforeOK: 1,
		failCode:     codes.Canceled,
		failMsg:      "the client connection is closing",
	}
	addr, stop := startRetryServer(t, srv)
	defer stop()

	gs, err := NewGRPCShard("test-shard", addr, "")
	if err != nil {
		t.Fatalf("NewGRPCShard: %v", err)
	}
	defer gs.Close()

	rows, err := gs.Query("SELECT 1")
	if err != nil {
		t.Fatalf("Query after retry: %v", err)
	}
	if len(rows) != 1 || rows[0].Raw != "after-retry" {
		t.Errorf("rows = %v, want one row with Raw=after-retry", rows)
	}
	if got := srv.calls.Load(); got != 2 {
		t.Errorf("server saw %d calls, want 2 (one fail + one retry)", got)
	}
}

// TestGRPCShard_Query_RetriesOnUnavailable covers the close-before-
// first-send variant: gRPC reports `Unavailable` when the underlying
// conn drops between the client's send and the server's first reply.
func TestGRPCShard_Query_RetriesOnUnavailable(t *testing.T) {
	srv := &retryServer{
		failBeforeOK: 1,
		failCode:     codes.Unavailable,
		failMsg:      "connection refused",
	}
	addr, stop := startRetryServer(t, srv)
	defer stop()

	gs, err := NewGRPCShard("test-shard", addr, "")
	if err != nil {
		t.Fatalf("NewGRPCShard: %v", err)
	}
	defer gs.Close()

	rows, err := gs.Query("SELECT 1")
	if err != nil {
		t.Fatalf("Query after retry: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %v, want 1", len(rows))
	}
}

// TestGRPCShard_Query_DoesNotRetryNonTransient verifies we don't
// retry classes the caller would consider deterministic — e.g.
// `InvalidArgument` on a malformed SQL. Retrying those wastes time
// and hides the real error.
func TestGRPCShard_Query_DoesNotRetryNonTransient(t *testing.T) {
	srv := &retryServer{
		failBeforeOK: 100, // would fail forever if retried
		failCode:     codes.InvalidArgument,
		failMsg:      "bad sql",
	}
	addr, stop := startRetryServer(t, srv)
	defer stop()

	gs, err := NewGRPCShard("test-shard", addr, "")
	if err != nil {
		t.Fatalf("NewGRPCShard: %v", err)
	}
	defer gs.Close()

	_, err = gs.Query("BAD")
	if err == nil {
		t.Fatal("expected error for InvalidArgument, got nil")
	}
	if !strings.Contains(err.Error(), "bad sql") {
		t.Errorf("error %q should preserve InvalidArgument message", err)
	}
	if got := srv.calls.Load(); got != 1 {
		t.Errorf("server saw %d calls, want 1 (no retry for non-transient)", got)
	}
}

// TestGRPCShard_Query_GivesUpAfterMaxAttempts puts an upper bound on
// the retry budget so a permanently-broken shard doesn't hold the
// query thread indefinitely.
func TestGRPCShard_Query_GivesUpAfterMaxAttempts(t *testing.T) {
	srv := &retryServer{
		failBeforeOK: 100,
		failCode:     codes.Canceled,
		failMsg:      "always failing",
	}
	addr, stop := startRetryServer(t, srv)
	defer stop()

	gs, err := NewGRPCShard("test-shard", addr, "")
	if err != nil {
		t.Fatalf("NewGRPCShard: %v", err)
	}
	defer gs.Close()

	_, err = gs.Query("SELECT 1")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	calls := srv.calls.Load()
	if calls < 2 || calls > 5 {
		t.Errorf("server saw %d calls, expected 2-5 attempts", calls)
	}
}

// TestGRPCShard_QueryToSink_RetriesInitialSetup verifies the
// streaming variant retries when the *initial* stream creation fails.
// Mid-stream errors must NOT retry (would double-send rows to the
// sink); that path stays exactly as-is.
func TestGRPCShard_QueryToSink_RetriesInitialSetup(t *testing.T) {
	srv := &retryServer{
		failBeforeOK: 1,
		failCode:     codes.Canceled,
		failMsg:      "the client connection is closing",
	}
	addr, stop := startRetryServer(t, srv)
	defer stop()

	gs, err := NewGRPCShard("test-shard", addr, "")
	if err != nil {
		t.Fatalf("NewGRPCShard: %v", err)
	}
	defer gs.Close()

	sink := &captureSink{}
	gs.QueryToSink("SELECT 1", nil, sink)

	if !sink.sawSentinel {
		t.Error("sink never saw sentinel")
	}
	if len(sink.rows) != 1 {
		t.Errorf("sink got %d rows, want 1", len(sink.rows))
	}
}

// captureSink is a minimal RowSink for assertions.
type captureSink struct {
	rows        []*pb.ResultRow
	sawSentinel bool
}

func (s *captureSink) SendRow(r *pb.ResultRow, _ string) { s.rows = append(s.rows, r) }
func (s *captureSink) SendSentinel(_ string)             { s.sawSentinel = true }
