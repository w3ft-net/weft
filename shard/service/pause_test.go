package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/graybeardcorp/weft/internal/logtest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// fakeVolume stands in for the EBS data volume: Mount blocks until the
// test attaches it (a spare waiting for the leader's AttachVolume), and
// both calls are counted so a test can assert the shard unmounted before
// reporting itself drained.
type fakeVolume struct {
	mu       sync.Mutex
	attached chan struct{}
	mounts   int
	unmounts int
}

func newFakeVolume(attached bool) *fakeVolume {
	v := &fakeVolume{attached: make(chan struct{})}
	if attached {
		close(v.attached)
	}
	return v
}

func (v *fakeVolume) attach() { close(v.attached) }

func (v *fakeVolume) Mount(ctx context.Context) error {
	select {
	case <-v.attached:
	case <-ctx.Done():
		return ctx.Err()
	}
	v.mu.Lock()
	v.mounts++
	v.mu.Unlock()
	return nil
}

func (v *fakeVolume) Unmount(context.Context) error {
	v.mu.Lock()
	v.unmounts++
	v.mu.Unlock()
	return nil
}

func (v *fakeVolume) counts() (mounts, unmounts int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mounts, v.unmounts
}

// seededServer starts a shard from identity on disk, the way an upgraded
// shard's volume looks, with vol as its data volume.
func seededServer(t *testing.T, vol Volume) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{
		ShardID:     "shard-pause",
		WindowStart: time.Unix(1000, 0).UTC(),
		WindowEnd:   time.Unix(2000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerFromIdentity(dir, filepath.Join(dir, "shard.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Volume = vol
	t.Cleanup(func() {
		if ds := srv.Shard(); ds != nil {
			ds.Close()
		}
	})
	return srv, dir
}

// serve runs srv behind its own interceptors on an in-memory listener and
// returns a client for it.
func serve(t *testing.T, srv *Server) pb.ShardQueryClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(srv.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(srv.StreamInterceptor()),
	)
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewShardQueryClient(conn)
}

func ingestOne(ctx context.Context, c pb.ShardQueryClient) error {
	_, err := c.Ingest(ctx, &pb.IngestRequest{Rows: []*pb.LogRow{{
		Raw: "hello", TsUnixNs: time.Unix(1500, 0).UnixNano(), Service: "svc",
	}}})
	return err
}

// A call that arrives while the shard is paused is refused before its
// handler runs: an Ingest leaves the row count unchanged, and the refusal
// names the successor so the caller can retry there.
func TestPause_RefusesNewCallsBeforeTheHandler(t *testing.T) {
	vol := newFakeVolume(true)
	srv, _ := seededServer(t, vol)
	c := serve(t, srv)
	ctx := context.Background()

	if err := ingestOne(ctx, c); err != nil {
		t.Fatalf("ingest while serving: %v", err)
	}
	before := srv.Shard().RowCount()

	if _, err := c.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "10.0.0.9:9000"}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	err := ingestOne(ctx, c)
	kind, succ := shard.ParseRefusal(err)
	if kind != shard.RefusedPaused || succ != "10.0.0.9:9000" {
		t.Fatalf("ingest while paused: got (%v, %q, %v), want a paused refusal naming 10.0.0.9:9000", kind, succ, err)
	}
	if srv.Shard() != nil {
		t.Fatal("Pause returned with the database still open")
	}
	if _, unmounts := vol.counts(); unmounts != 1 {
		t.Fatalf("Pause returned without unmounting the volume (unmounts=%d)", unmounts)
	}

	// The refused Ingest never reached DuckDB: reopen and count.
	if _, err := c.Resume(ctx, &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := srv.Shard().RowCount(); got != before {
		t.Fatalf("row count after a refused ingest: got %d, want %d", got, before)
	}

	// A refused streaming Query fails before its first message too.
	if _, err := c.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "10.0.0.9:9000"}); err != nil {
		t.Fatalf("second Pause: %v", err)
	}
	stream, err := c.Query(ctx, &pb.QueryRequest{Sql: "SELECT raw, ts FROM logs"})
	if err == nil {
		_, err = stream.Recv()
	}
	if kind, _ := shard.ParseRefusal(err); kind != shard.RefusedPaused {
		t.Fatalf("query while paused: got %v, want a paused refusal", err)
	}
}

// Pause waits for the calls already admitted: an in-flight call runs to
// completion before Pause reports the shard drained.
func TestPause_WaitsForInFlightCalls(t *testing.T) {
	// Writes are never cut, however long they outlive the query grace.
	shrinkQueryGrace(t, 10*time.Millisecond)
	srv, _ := seededServer(t, newFakeVolume(true))
	unary := srv.UnaryInterceptor()

	release := make(chan struct{})
	entered := make(chan struct{})
	handlerDone := make(chan error, 1)
	go func() {
		_, err := unary(context.Background(), &pb.IngestRequest{}, &grpc.UnaryServerInfo{FullMethod: pb.ShardQuery_Ingest_FullMethodName},
			func(ctx context.Context, req any) (any, error) {
				close(entered)
				<-release
				return &pb.IngestResponse{}, nil
			})
		handlerDone <- err
	}()
	<-entered

	paused := make(chan error, 1)
	go func() {
		_, err := srv.Pause(context.Background(), &pb.PauseRequest{SuccessorAddr: "succ:9000"})
		paused <- err
	}()
	select {
	case err := <-paused:
		t.Fatalf("Pause returned (%v) while a call was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	// A new call is refused even though the old one is still running.
	_, err := unary(context.Background(), &pb.IngestRequest{}, &grpc.UnaryServerInfo{FullMethod: pb.ShardQuery_Ingest_FullMethodName},
		func(context.Context, any) (any, error) {
			t.Fatal("handler ran for a call admitted after Pause")
			return nil, nil
		})
	if kind, _ := shard.ParseRefusal(err); kind != shard.RefusedPaused {
		t.Fatalf("call during drain: got %v, want a paused refusal", err)
	}

	close(release)
	if err := <-handlerDone; err != nil {
		t.Fatalf("in-flight call: %v", err)
	}
	select {
	case err := <-paused:
		if err != nil {
			t.Fatalf("Pause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pause never returned after the in-flight call finished")
	}
}

// Pause is idempotent (the leader retries it after a lost reply), and its
// paused state stops the heartbeat that would otherwise keep announcing
// the old address.
func TestPause_IsIdempotentAndSilencesHeartbeats(t *testing.T) {
	vol := newFakeVolume(true)
	srv, _ := seededServer(t, vol)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := srv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "succ:9000"}); err != nil {
			t.Fatalf("Pause #%d: %v", i+1, err)
		}
	}
	if _, unmounts := vol.counts(); unmounts != 1 {
		t.Fatalf("unmounts after two Pauses: got %d, want 1", unmounts)
	}
	if !srv.Paused() {
		t.Fatal("Paused() is false after Pause")
	}
	if _, err := srv.Resume(ctx, &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if srv.Paused() {
		t.Fatal("Paused() is true after Resume")
	}
	if _, err := srv.Resume(ctx, &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume on a serving shard must be a no-op: %v", err)
	}
	if mounts, _ := vol.counts(); mounts != 1 {
		t.Fatalf("mounts after Resume, Resume: got %d, want 1", mounts)
	}
}

// A spare holds no volume: it refuses every data call as not ready and
// heartbeats as a spare. Once the volume is attached it mounts it, opens
// the shard it finds there, and serves under that identity.
func TestSpare_RefusesUntilItsVolumeIsAttached(t *testing.T) {
	// Lay down a shard's volume contents, as the upgrade's old instance left them.
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{ShardID: "shard-moved", HeartbeatToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	vol := newFakeVolume(false)
	srv := NewSpareServer(dir, filepath.Join(dir, "shard.duckdb"), vol)
	t.Cleanup(func() {
		if ds := srv.Shard(); ds != nil {
			ds.Close()
		}
	})
	c := serve(t, srv)
	ctx := context.Background()

	if !srv.Spare() {
		t.Fatal("a fresh spare must report Spare()")
	}
	if srv.Identity() != "" {
		t.Fatalf("a spare must heartbeat with no shard id, got %q", srv.Identity())
	}
	if kind, _ := shard.ParseRefusal(ingestOne(ctx, c)); kind != shard.RefusedNotReady {
		t.Fatal("a spare must refuse ingest as not ready")
	}
	_, err := c.Configure(ctx, &pb.ConfigureRequest{ShardId: "shard-other"})
	if kind, _ := shard.ParseRefusal(err); kind != shard.RefusedNotReady {
		t.Fatalf("a spare must refuse Configure as not ready (it could christen the wrong volume), got %v", err)
	}

	activated := make(chan error, 1)
	go func() { activated <- srv.Activate(ctx) }()
	vol.attach()
	if err := <-activated; err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if srv.Spare() || srv.Identity() != "shard-moved" || srv.HeartbeatToken() != "tok" {
		t.Fatalf("after activation: spare=%v id=%q token=%q", srv.Spare(), srv.Identity(), srv.HeartbeatToken())
	}
	if err := ingestOne(ctx, c); err != nil {
		t.Fatalf("ingest after activation: %v", err)
	}
}

// A spare claimed for a brand-new shard gets a blank volume: it activates
// with no identity and waits for Configure like any fresh shard.
func TestSpare_ActivatesBlankForAFreshVolume(t *testing.T) {
	dir := t.TempDir()
	srv := NewSpareServer(dir, filepath.Join(dir, "shard.duckdb"), newFakeVolume(true))
	t.Cleanup(func() {
		if ds := srv.Shard(); ds != nil {
			ds.Close()
		}
	})
	c := serve(t, srv)
	ctx := context.Background()
	if err := srv.Activate(ctx); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if srv.Spare() || srv.Identity() != "" {
		t.Fatalf("blank activation: spare=%v id=%q", srv.Spare(), srv.Identity())
	}
	if _, err := c.Configure(ctx, &pb.ConfigureRequest{ShardId: "shard-new"}); err != nil {
		t.Fatalf("Configure after blank activation: %v", err)
	}
	if err := ingestOne(ctx, c); err != nil {
		t.Fatalf("ingest after Configure: %v", err)
	}
}

func shrinkQueryGrace(t *testing.T, d time.Duration) {
	prev := pauseQueryGrace
	pauseQueryGrace = d
	t.Cleanup(func() { pauseQueryGrace = prev })
}

// queryStream is the server side of a Query stream, for driving the
// stream interceptor directly.
type queryStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (q *queryStream) Context() context.Context { return q.ctx }
func (q *queryStream) SendMsg(any) error        { return nil }

// startLongQuery admits a Query that runs until its context is cancelled,
// sending one row first when sendRow is set, and returns what the
// interceptor answers it with.
func startLongQuery(t *testing.T, srv *Server, sendRow bool) <-chan error {
	t.Helper()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- srv.StreamInterceptor()(nil, &queryStream{ctx: context.Background()},
			&grpc.StreamServerInfo{FullMethod: pb.ShardQuery_Query_FullMethodName},
			func(_ any, ss grpc.ServerStream) error {
				if sendRow {
					if err := ss.SendMsg(&pb.ResultRow{}); err != nil {
						return err
					}
				}
				close(entered)
				<-ss.Context().Done()
				return ss.Context().Err()
			})
	}()
	<-entered
	return done
}

// A query still running when the grace runs out is cut so the pause can
// finish. Having sent nothing, it is refused as paused, naming the
// successor: the caller re-runs it there.
func TestPause_CutsAQueryThatOutlivesTheGrace(t *testing.T) {
	shrinkQueryGrace(t, 50*time.Millisecond)
	srv, _ := seededServer(t, newFakeVolume(true))
	answered := startLongQuery(t, srv, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := srv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "succ:9000"}); err != nil {
		t.Fatalf("Pause with a long query in flight: %v", err)
	}
	err := <-answered
	if kind, succ := shard.ParseRefusal(err); kind != shard.RefusedPaused || succ != "succ:9000" {
		t.Fatalf("cut query: got %v, want a paused refusal naming succ:9000", err)
	}
}

// A query cut after it has streamed rows can't be re-run without sending
// them twice, so it fails rather than being refused.
func TestPause_CutsAStreamingQueryAsAFailure(t *testing.T) {
	shrinkQueryGrace(t, 50*time.Millisecond)
	srv, _ := seededServer(t, newFakeVolume(true))
	answered := startLongQuery(t, srv, true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := srv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "succ:9000"}); err != nil {
		t.Fatalf("Pause with a streaming query in flight: %v", err)
	}
	err := <-answered
	if kind, _ := shard.ParseRefusal(err); kind != shard.NotRefused || status.Code(err) != codes.Unavailable {
		t.Fatalf("cut streaming query: got %v, want a plain Unavailable failure", err)
	}
}

// A Pause that can't drain within its deadline fails, and leaves the shard
// paused rather than half-closed, so the leader's Resume brings it back.
func TestPause_FailsWhenDrainOutlivesItsDeadline(t *testing.T) {
	srv, _ := seededServer(t, newFakeVolume(true))
	unary := srv.UnaryInterceptor()
	release := make(chan struct{})
	entered := make(chan struct{})
	go unary(context.Background(), &pb.IngestRequest{}, &grpc.UnaryServerInfo{FullMethod: pb.ShardQuery_Ingest_FullMethodName},
		func(context.Context, any) (any, error) {
			close(entered)
			<-release
			return &pb.IngestResponse{}, nil
		})
	<-entered
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := srv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "succ:9000"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pause with a stuck call: got %v, want a deadline error", err)
	}
	if srv.Shard() == nil {
		t.Fatal("a Pause that failed to drain closed the database anyway")
	}
	if _, err := srv.Resume(context.Background(), &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume after a failed Pause: %v", err)
	}
	if srv.Paused() {
		t.Fatal("still paused after Resume")
	}
}

// Pause's slow steps are spans in the trace of the roll that asked for it, so
// a hold window breaks down by step in `trace --spans`.
func TestPause_RecordsItsStepsAsSpans(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	srv, _ := seededServer(t, newFakeVolume(true))
	ctx := weftsdk.ContextWithTraceID(context.Background(), traceID)

	out := logtest.Capture(func() {
		if _, err := srv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: "10.0.0.9:9000"}); err != nil {
			t.Fatalf("Pause: %v", err)
		}
	})
	done := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) != nil || rec["msg"] != "span done" {
			continue
		}
		if rec["trace_id"] != traceID {
			t.Errorf("span outside the roll's trace: %s", l)
		}
		name, _ := rec["span_name"].(string)
		done[name] = true
	}
	for _, name := range []string{"pause.drain", "pause.checkpoint", "pause.unmount"} {
		if !done[name] {
			t.Errorf("no %q span in Pause's log:\n%s", name, out)
		}
	}
}

// A roll keeps the shard's field facts: they live on the volume, Pause lets the
// classifier finish first, and the instance that opens it next has them without
// rescanning a row.
func TestPause_KeepsFieldFactsForTheNextInstance(t *testing.T) {
	vol := newFakeVolume(true)
	srv, dir := seededServer(t, vol)
	rows := make([]*pb.LogRow, 1000)
	for i := range rows {
		rows[i] = &pb.LogRow{Raw: "x 7", TsUnixNs: time.Unix(1500, 0).UnixNano(), Service: "svc", Namespace: "ns", CustomerId: 1}
	}
	if err := srv.Shard().Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Pause(context.Background(), &pb.PauseRequest{SuccessorAddr: "succ:9000"}); err != nil {
		t.Fatal(err)
	}

	next, err := shard.NewDuckDBShard("shard-pause", filepath.Join(dir, "shard.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	var typed bool
	for _, f := range next.FieldFacts() {
		typed = typed || (f.Locator == "$2" && f.State == shard.FactTyped)
	}
	if !typed {
		t.Errorf("successor has no typed $2 after the roll: %+v", next.FieldFacts())
	}
}
