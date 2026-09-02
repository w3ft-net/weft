package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	ingsyslog "github.com/graybeardcorp/weft/ingest/syslog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// fakeIngestCoord stands in for the coordinator's IngestStream RPC —
// the same path weftd, JSONAdapter, and HerokuAdapter all forward
// into. Records every row received and the "authorization" metadata
// each stream carried, so tests can assert SyslogServer forwarded the
// connection's token unchanged.
type fakeIngestCoord struct {
	pb.UnimplementedWeftCoordinatorServer

	mu       sync.Mutex
	rows     []*pb.LogRow
	authSeen []string
	calls    int
}

func (f *fakeIngestCoord) IngestStream(stream grpc.ClientStreamingServer[pb.LogRow, pb.IngestStreamResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.calls++
	if vals := md.Get("authorization"); len(vals) > 0 {
		f.authSeen = append(f.authSeen, vals[0])
	}
	f.mu.Unlock()
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.IngestStreamResponse{})
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.rows = append(f.rows, row)
		f.mu.Unlock()
	}
}

func (f *fakeIngestCoord) snapshot() (rows []*pb.LogRow, authSeen []string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.LogRow(nil), f.rows...), append([]string(nil), f.authSeen...), f.calls
}

// startFakeCoord dials a fresh gRPC server implementing only
// IngestStream, returning it alongside a ready client. Mirrors the
// fakeCoord pattern in shell_test.go.
func startFakeCoord(t *testing.T) (*fakeIngestCoord, pb.WeftCoordinatorClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeIngestCoord{}
	grpcSrv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(grpcSrv, fake)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake coord: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return fake, pb.NewWeftCoordinatorClient(conn)
}

// startTestSyslogServer wires a SyslogServer with RenderProducer against
// an ephemeral port and returns it started, along with its bound
// address to dial.
func startTestSyslogServer(t *testing.T, srv *Server) (*SyslogServer, string) {
	t.Helper()
	ss := srv.NewSyslogServer(ingsyslog.RenderProducer{}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	if err := ss.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start syslog server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		ss.Close()
	})
	return ss, ss.ln.Addr().String()
}

func createOrgAndProducerToken(t *testing.T, s *store.Store, producer string) string {
	t.Helper()
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("syslog-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	ns, err := s.FindOrCreateNamespace(cust.ID, "test")
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	raw, _, err := s.CreateAgentToken(cust.ID, ns.ID, "syslog-token", "", producer)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw
}

// waitForRows polls fake until n rows arrive or the timeout expires.
func waitForRows(t *testing.T, fake *fakeIngestCoord, n int) []*pb.LogRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _, _ := fake.snapshot()
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSyslogServer_ValidatesAndForwards is the golden path: a
// producer-scoped token, three RFC5424 frames each carrying the
// leading "TOKEN SP" prefix the wire capture confirmed (see
// docs/ingestion-syslog.md, "Wire-capture first, always"). Two are
// real service output and should be forwarded field-mapped; the third
// is Render's own connectivity-test ping (HOSTNAME "render") and
// RenderProducer must drop it.
func TestSyslogServer_ValidatesAndForwards(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	raw := createOrgAndProducerToken(t, s, "render")
	_, addr := startTestSyslogServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial syslog server: %v", err)
	}
	defer conn.Close()

	lines := []string{
		raw + " <6>1 2026-09-02T14:08:25.185717Z render-capture-testsvc web-9nk6q 1 web-9nk6q - heartbeat #163 hit a deliberate exception\n",
		raw + " <14>1 2026-09-02T14:08:26.000000Z render-capture-testsvc cron-stqq5 1 cron-stqq5 - cron ran to completion\n",
		raw + " <6>1 2026-09-02T14:08:27.000000Z render web-9nk6q 1 web-9nk6q - connectivity check\n",
	}
	for _, l := range lines {
		if _, err := conn.Write([]byte(l)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	conn.Close() // EOF ends the connection's read loop

	rows := waitForRows(t, fake, 2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Service != "render-capture-testsvc" || rows[0].Kind != "web" || rows[0].Instance != "9nk6q" {
		t.Errorf("row 0 = %+v, want service=render-capture-testsvc kind=web instance=9nk6q", rows[0])
	}
	if rows[0].Raw != "heartbeat #163 hit a deliberate exception" {
		t.Errorf("row 0 raw = %q", rows[0].Raw)
	}
	if rows[1].Kind != "cron" || rows[1].Instance != "stqq5" {
		t.Errorf("row 1 = %+v, want kind=cron instance=stqq5", rows[1])
	}

	_, authSeen, _ := fake.snapshot()
	if len(authSeen) == 0 || authSeen[0] != "Bearer "+raw {
		t.Errorf("authSeen = %v, want [%q]", authSeen, "Bearer "+raw)
	}
}

// TestSyslogServer_RejectsUnscopedToken confirms a token minted with
// no producer (every weftd/JSON/Heroku token) cannot authenticate a
// syslog connection even though it's otherwise valid and unrevoked —
// the whole point of the Producer-scoping added in migration 073.
func TestSyslogServer_RejectsUnscopedToken(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	raw := createOrgAndProducerToken(t, s, "") // unscoped
	_, addr := startTestSyslogServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial syslog server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(raw + " <6>1 2026-09-02T14:08:25Z test-service web-1 1 web-1 - hi\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the server to close the connection on an unscoped token")
	}

	if _, _, calls := fake.snapshot(); calls != 0 {
		t.Errorf("coordinator IngestStream should never have been called, got %d calls", calls)
	}
}

// TestSyslogServer_RejectsWrongProducerToken confirms a token scoped
// to a different producer (a hypothetical second platform) can't be
// replayed against this SyslogServer instance.
func TestSyslogServer_RejectsWrongProducerToken(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient

	raw := createOrgAndProducerToken(t, s, "flyio")
	_, addr := startTestSyslogServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial syslog server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(raw + " <6>1 2026-09-02T14:08:25Z test-service web-1 1 web-1 - hi\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the server to close the connection on a wrong-producer token")
	}

	if _, _, calls := fake.snapshot(); calls != 0 {
		t.Errorf("coordinator IngestStream should never have been called, got %d calls", calls)
	}
}

// TestSyslogServer_RejectsMissingTokenPrefix covers the "no Token
// configured in Render" case: frames arrive bare, starting directly
// with '<'. SyslogServer rejects rather than guessing a namespace,
// matching every other ingest adapter.
func TestSyslogServer_RejectsMissingTokenPrefix(t *testing.T) {
	srv, s := testServer(t)
	fake, coordClient := startFakeCoord(t)
	srv.coordClient = coordClient
	createOrgAndProducerToken(t, s, "render") // exists, but never presented

	_, addr := startTestSyslogServer(t, srv)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial syslog server: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("<6>1 2026-09-02T14:08:25Z test-service web-1 1 web-1 - hi\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the server to close the connection with no token prefix")
	}

	if _, _, calls := fake.snapshot(); calls != 0 {
		t.Errorf("coordinator IngestStream should never have been called, got %d calls", calls)
	}
}
