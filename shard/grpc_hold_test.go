package shard_test

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/w3ft-net/weft/logtest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// gatedVolume is a data volume whose Mount blocks until the test attaches
// it: a successor that is still waiting for the leader's AttachVolume.
type gatedVolume struct {
	once     sync.Once
	attached chan struct{}
}

func newGatedVolume() *gatedVolume { return &gatedVolume{attached: make(chan struct{})} }

func (v *gatedVolume) attach() { v.once.Do(func() { close(v.attached) }) }

func (v *gatedVolume) Mount(ctx context.Context) error {
	select {
	case <-v.attached:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *gatedVolume) Unmount(context.Context) error { return nil }

// liveShard runs srv behind its gate on a real TCP port and returns the
// address and a stop func.
func liveShard(t *testing.T, srv *shardsvc.Server) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(srv.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(srv.StreamInterceptor()),
	)
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	var once sync.Once
	stop := func() { once.Do(gs.Stop) }
	t.Cleanup(stop)
	return lis.Addr().String(), stop
}

// upgradePair stands up the two halves of an upgrade: the old shard
// serving shard-hold from its own volume, and a spare whose (separate, for
// the test) data directory carries the same identity and opens once the
// test attaches its volume.
func upgradePair(t *testing.T) (oldSrv, spare *shardsvc.Server, oldAddr, spareAddr string, attach func(), stopSpare func()) {
	t.Helper()
	ident := &shard.Identity{ShardID: "shard-hold"}
	oldDir, spareDir := t.TempDir(), t.TempDir()
	for _, d := range []string{oldDir, spareDir} {
		if err := shard.SaveIdentity(d, ident); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	oldSrv, err = shardsvc.NewServerFromIdentity(oldDir, filepath.Join(oldDir, "shard.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	vol := newGatedVolume()
	spare = shardsvc.NewSpareServer(spareDir, filepath.Join(spareDir, "shard.duckdb"), vol)
	go spare.Activate(context.Background())
	t.Cleanup(func() {
		for _, s := range []*shardsvc.Server{oldSrv, spare} {
			if ds := s.Shard(); ds != nil {
				ds.Close()
			}
		}
	})
	oldAddr, _ = liveShard(t, oldSrv)
	spareAddr, stopSpare = liveShard(t, spare)
	return oldSrv, spare, oldAddr, spareAddr, vol.attach, stopSpare
}

func row(raw string) *pb.LogRow {
	return &pb.LogRow{Raw: raw, Service: "svc", TsUnixNs: time.Unix(1500, 0).UnixNano()}
}

type collectSink struct {
	mu     sync.Mutex
	rows   []*pb.ResultRow
	failed error
}

func (s *collectSink) SendRow(r *pb.ResultRow, _ string) {
	s.mu.Lock()
	s.rows = append(s.rows, r)
	s.mu.Unlock()
}
func (s *collectSink) Fail(_ string, err error) { s.mu.Lock(); s.failed = err; s.mu.Unlock() }
func (s *collectSink) SendSentinel(string)      {}

// A Query and an Ingest issued while the shard is paused both land on the
// successor once it opens, each exactly once: the client follows the
// redirect on the same handle and holds while the successor isn't ready.
func TestGRPCShard_HoldsThroughAPauseAndLandsOnTheSuccessor(t *testing.T) {
	oldSrv, spare, oldAddr, spareAddr, attach, _ := upgradePair(t)
	gs, err := shard.NewGRPCShard("shard-hold", oldAddr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()
	ctx := context.Background()
	if err := gs.Ingest(ctx, []*pb.LogRow{row("before")}, nil); err != nil {
		t.Fatalf("ingest before the pause: %v", err)
	}

	if _, err := oldSrv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: spareAddr}); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	ingested := make(chan error, 1)
	go func() { ingested <- gs.Ingest(ctx, []*pb.LogRow{row("during")}, nil) }()
	sink := &collectSink{}
	queried := make(chan struct{})
	go func() {
		gs.QueryToSink(ctx, "SELECT raw, ts FROM logs", nil, sink)
		close(queried)
	}()

	// Neither call can finish before the successor opens.
	select {
	case err := <-ingested:
		t.Fatalf("ingest finished (%v) before the successor was ready", err)
	case <-queried:
		t.Fatalf("query finished before the successor was ready (failed=%v)", sink.failed)
	case <-time.After(300 * time.Millisecond):
	}

	out := logtest.Capture(func() {
		attach()
		select {
		case err := <-ingested:
			if err != nil {
				t.Fatalf("held ingest: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("held ingest never completed on the successor")
		}
		<-queried
	})
	// Each held call reports what the pause cost it.
	for _, op := range []string{"Ingest", "QueryToSink"} {
		if !strings.Contains(out, op+" held ") || !strings.Contains(out, "went through at "+spareAddr) {
			t.Fatalf("no hold line for %s in the log:\n%s", op, out)
		}
	}
	if sink.failed != nil {
		t.Fatalf("held query failed: %v", sink.failed)
	}
	if got := spare.Shard().RowCount(); got != 1 {
		t.Fatalf("successor row count: got %d, want exactly the one held ingest", got)
	}
	if gs.Addr() != spareAddr {
		t.Fatalf("handle still points at %s, want the successor %s", gs.Addr(), spareAddr)
	}
}

// Past the hold timeout a call fails; a query reports its rows missing
// rather than answering without them.
func TestGRPCShard_HoldGivesUpAfterTheTimeout(t *testing.T) {
	defer shard.SetPauseHoldTimeoutForTest(200 * time.Millisecond)()
	oldSrv, _, oldAddr, spareAddr, _, _ := upgradePair(t)
	gs, err := shard.NewGRPCShard("shard-hold", oldAddr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()
	ctx := context.Background()
	if _, err := oldSrv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: spareAddr}); err != nil {
		t.Fatal(err)
	}

	sink := &collectSink{}
	done := make(chan struct{})
	go func() { gs.QueryToSink(ctx, "SELECT raw, ts FROM logs", nil, sink); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("query held past its timeout")
	}
	if sink.failed == nil {
		t.Fatal("a query whose shard never came back must fail, not return empty")
	}
	if err := gs.Ingest(ctx, []*pb.LogRow{row("x")}, nil); err == nil {
		t.Fatal("an ingest whose shard never came back must fail")
	}
}

// A rollback: the successor never readies and is torn down, the old shard
// resumes. A call held against the successor goes back to the old address
// and completes there.
func TestGRPCShard_HeldCallReturnsHomeOnRollback(t *testing.T) {
	oldSrv, _, oldAddr, spareAddr, _, stopSpare := upgradePair(t)
	gs, err := shard.NewGRPCShard("shard-hold", oldAddr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()
	ctx := context.Background()
	if _, err := oldSrv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: spareAddr}); err != nil {
		t.Fatal(err)
	}
	ingested := make(chan error, 1)
	go func() { ingested <- gs.Ingest(ctx, []*pb.LogRow{row("held")}, nil) }()
	time.Sleep(300 * time.Millisecond) // the call is now holding against the spare

	stopSpare()
	if _, err := oldSrv.Resume(ctx, &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case err := <-ingested:
		if err != nil {
			t.Fatalf("held ingest after rollback: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("held ingest never returned to the old shard")
	}
	if got := oldSrv.Shard().RowCount(); got != 1 {
		t.Fatalf("old shard row count after rollback: got %d, want 1", got)
	}
}

// A held call that ends before the upgrade does (its caller's deadline passed)
// leaves the handle redirected at the successor. If the upgrade then rolls back
// and the successor is torn down, calls made later start at that dead address:
// they must find their way home, not fail on it for as long as the handle lives
// (a compute worker keeps its handles for the life of the process, and the
// coordinator's descriptor still names the old address, so nothing repoints it).
func TestGRPCShard_ACallAfterARollbackGoesHomeEvenIfTheHoldEndedEarlier(t *testing.T) {
	oldSrv, _, oldAddr, spareAddr, _, stopSpare := upgradePair(t)
	gs, err := shard.NewGRPCShard("shard-hold", oldAddr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()
	ctx := context.Background()
	if _, err := oldSrv.Pause(ctx, &pb.PauseRequest{SuccessorAddr: spareAddr}); err != nil {
		t.Fatal(err)
	}

	// The first call holds against the spare and gives up when its own deadline
	// passes, with the handle still pointing there.
	held, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	if _, err := gs.Query(held, "SELECT raw, ts FROM logs"); err == nil {
		t.Fatal("a call into a paused shard returned without holding")
	}
	cancel()
	if got := gs.Addr(); got != spareAddr {
		t.Fatalf("handle is at %s after the held call, want the successor %s", got, spareAddr)
	}

	// Rollback: the spare is torn down, the old shard resumes.
	stopSpare()
	if _, err := oldSrv.Resume(ctx, &pb.ResumeRequest{}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	fresh, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := gs.Query(fresh, "SELECT raw, ts FROM logs"); err != nil {
		t.Fatalf("a call after the rollback failed on the dead successor: %v", err)
	}
	if got := gs.Addr(); got != oldAddr {
		t.Errorf("handle still at %s after the rollback, want home %s", got, oldAddr)
	}
}

// Repoint moves a handle to a shard's new address in place, the way a
// coordinator follows a shard_records addr change without redialing.
func TestGRPCShard_RepointMovesTheHandle(t *testing.T) {
	_, spare, oldAddr, spareAddr, attach, _ := upgradePair(t)
	gs, err := shard.NewGRPCShard("shard-hold", oldAddr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer gs.Close()
	attach()
	gs.Repoint(spareAddr)
	if gs.Addr() != spareAddr {
		t.Fatalf("Addr after Repoint: %s", gs.Addr())
	}
	if err := gs.Ingest(context.Background(), []*pb.LogRow{row("moved")}, nil); err != nil {
		t.Fatalf("ingest after Repoint: %v", err)
	}
	if got := spare.Shard().RowCount(); got != 1 {
		t.Fatalf("successor row count after Repoint: got %d, want 1", got)
	}
}
