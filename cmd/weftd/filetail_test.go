package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/weftd"
)

// These tests drive the real runFileTailer against an in-process fake
// coordinator that, like the real one, treats rows as ingested the moment
// they are Sent but only acknowledges them at CloseAndRecv. That gap is what
// the checkpoint must not fall into. See docs/plans/weftd-offset-checkpointing.md.

type fakeCoord struct {
	pb.WeftCoordinatorClient
	mu       sync.Mutex
	received []string // every Raw ever Sent (dupes allowed)
	opens    int
	failAcks atomic.Int32 // CloseAndRecv calls to fail (rows still count as received)
	hangAck  atomic.Bool  // CloseAndRecv blocks until its stream ctx is done
}

type fakeCoordStream struct {
	grpc.ClientStream
	c   *fakeCoord
	ctx context.Context
}

func (c *fakeCoord) IngestStream(ctx context.Context, _ ...grpc.CallOption) (pb.WeftCoordinator_IngestStreamClient, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return &fakeCoordStream{c: c, ctx: ctx}, nil
}

func (s *fakeCoordStream) Send(row *pb.LogRow) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.c.mu.Lock()
	s.c.received = append(s.c.received, row.Raw)
	s.c.mu.Unlock()
	return nil
}

func (s *fakeCoordStream) CloseAndRecv() (*pb.IngestStreamResponse, error) {
	if s.c.hangAck.Load() {
		<-s.ctx.Done()
		return nil, s.ctx.Err()
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if s.c.failAcks.Add(-1) >= 0 {
		return nil, fmt.Errorf("ack lost")
	}
	return &pb.IngestStreamResponse{}, nil
}

func (c *fakeCoord) raws() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.received...)
}

func (c *fakeCoord) openCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

// uniq returns the sorted distinct values of raws.
func uniq(raws []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range raws {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// ckptDir is the checkpoint directory of the current test's store.
var ckptDir string

func ckptDirOf(t *testing.T) string { t.Helper(); return ckptDir }

// shipperEnv wires the package-level knobs for one test and restores them.
func shipperEnv(t *testing.T, dir string, cfg shipperConfig) *weftd.CheckpointStore {
	t.Helper()
	oldCfg, oldCk, oldG := shipCfg, checkpoints, tailers
	t.Cleanup(func() { shipCfg, checkpoints, tailers = oldCfg, oldCk, oldG })
	shipCfg = cfg
	tailers = &tailerGroup{}
	checkpoints = nil
	ckptDir = dir
	if dir != "" {
		c, err := weftd.OpenCheckpointStore(dir, weftd.CheckpointOptions{})
		if err != nil {
			t.Fatal(err)
		}
		checkpoints = c
	}
	return checkpoints
}

func fastCfg() shipperConfig {
	return shipperConfig{commitInterval: 100 * time.Millisecond, windowBytes: 8 << 20, shutdownTimeout: 2 * time.Second}
}

func appendLog(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitTailing blocks until the agent has opened path. A fresh install starts
// at EOF, so a test that appends before this would have its lines skipped.
func waitTailing(t *testing.T, path string) {
	t.Helper()
	if checkpoints == nil {
		time.Sleep(200 * time.Millisecond)
		return
	}
	key, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tailer to open "+path, func() bool {
		checkpoints.Flush()
		data, err := os.ReadFile(filepath.Join(ckptDirOf(t), "state.json"))
		return err == nil && strings.Contains(string(data), `"`+key+`"`)
	})
}

// runAgent starts runFileTailer and returns a stop func that performs the
// graceful shutdown main() does: cancel, wait for tailers, close checkpoints.
func runAgent(t *testing.T, coord *fakeCoord, path string, startup bool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFileTailer(ctx, coord, "host", "ns/app/main", path, startup)
	}()
	waitTailing(t, path)
	return func() {
		cancel()
		tailers.shutdown()
		<-done
		checkpoints.Close()
	}
}

func lines(prefix string, from, to int) string {
	var sb strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&sb, "%s-%03d\n", prefix, i)
	}
	return sb.String()
}

func want(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s-%03d", prefix, i))
	}
	return out
}

// Graceful shutdown must deliver what it read. Before, the final flush ran
// with the already-cancelled ctx, so lines in flight were dropped.
func TestFileTailer_GracefulShutdownDeliversInFlightLines(t *testing.T) {
	ck := t.TempDir()
	shipperEnv(t, ck, shipperConfig{commitInterval: time.Hour, windowBytes: 8 << 20, shutdownTimeout: 2 * time.Second})
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, true)
	appendLog(t, log, lines("pre", 1, 20))
	waitFor(t, "lines read", func() bool { return len(coord.raws()) == 20 })
	stop() // commit interval is an hour: only the shutdown drain can ack these

	// Restart: nothing in flight may be lost or replayed beyond dupes.
	shipperEnv(t, ck, fastCfg())
	stop = runAgent(t, coord, log, true)
	defer stop()
	appendLog(t, log, lines("post", 1, 5))
	waitFor(t, "post lines", func() bool { return len(uniq(coord.raws())) >= 25 })

	got := uniq(coord.raws())
	if !equalStrings(got, append(want("post", 1, 5), want("pre", 1, 20)...)) {
		t.Errorf("distinct lines = %v", got)
	}
	// The shutdown drain acked pre-*, so the restart must not replay them.
	for _, r := range coord.raws()[20:] {
		if strings.HasPrefix(r, "pre-") {
			t.Errorf("acked line %q was replayed after restart", r)
		}
	}
}

// Lines written while weftd is down are picked up from the checkpoint.
func TestFileTailer_RoundTripAcrossRestartHasNoGap(t *testing.T) {
	ck := t.TempDir()
	shipperEnv(t, ck, fastCfg())
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, true)
	appendLog(t, log, lines("a", 1, 10))
	waitFor(t, "first batch", func() bool { return len(coord.raws()) == 10 })
	stop()

	appendLog(t, log, lines("down", 1, 7)) // written while weftd is down

	shipperEnv(t, ck, fastCfg())
	stop = runAgent(t, coord, log, true)
	defer stop()
	appendLog(t, log, lines("b", 1, 5))
	waitFor(t, "second batch", func() bool { return len(uniq(coord.raws())) == 22 })

	got := uniq(coord.raws())
	exp := append(append(want("a", 1, 10), want("b", 1, 5)...), want("down", 1, 7)...)
	sort.Strings(exp)
	if !equalStrings(got, exp) {
		t.Errorf("lines = %v\nwant  %v", got, exp)
	}
}

// If the ack never arrives the checkpoint must not advance, so everything
// is delivered again after restart (dupes allowed, loss not).
func TestFileTailer_CrashBeforeAckRedeliversEverything(t *testing.T) {
	ck := t.TempDir()
	shipperEnv(t, ck, fastCfg())
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}
	coord.hangAck.Store(true) // rows are Sent (ingested) but no ack ever returns

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFileTailer(ctx, coord, "host", "ns/app/main", log, true)
	}()
	waitTailing(t, log)
	appendLog(t, log, lines("x", 1, 12))
	waitFor(t, "rows sent", func() bool { return len(coord.raws()) == 12 })
	// Simulate a kill: no graceful drain, and nothing was ever committed.
	cancel()
	<-done
	checkpoints.Close()

	coord.hangAck.Store(false)
	shipperEnv(t, ck, fastCfg())
	stop := runAgent(t, coord, log, true)
	defer stop()
	waitFor(t, "redelivery", func() bool {
		n := 0
		for _, r := range coord.raws() {
			if strings.HasPrefix(r, "x-") {
				n++
			}
		}
		return n >= 24
	})
	if got := uniq(coord.raws()); !equalStrings(got, want("x", 1, 12)) {
		t.Errorf("distinct = %v", got)
	}
}

// A failed ack retains the window and resends it; the checkpoint advances
// only once an ack succeeds.
func TestFileTailer_FailedAckResendsWindowThenCommits(t *testing.T) {
	ck := t.TempDir()
	shipperEnv(t, ck, fastCfg())
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}
	coord.failAcks.Store(2)

	stop := runAgent(t, coord, log, true)
	appendLog(t, log, lines("r", 1, 5))
	// 5 rows, then two failed acks each followed by a full resend = 15 sends.
	waitFor(t, "resends", func() bool { return len(coord.raws()) >= 15 })
	stop()

	if got := uniq(coord.raws()); !equalStrings(got, want("r", 1, 5)) {
		t.Errorf("distinct = %v", got)
	}
	// Eventually acked: a restart must not replay them.
	before := len(coord.raws())
	shipperEnv(t, ck, fastCfg())
	stop = runAgent(t, coord, log, true)
	time.Sleep(400 * time.Millisecond)
	stop()
	if after := len(coord.raws()); after != before {
		t.Errorf("acked lines were replayed after restart (%d -> %d)", before, after)
	}
}

// A full window forces an ack cycle without waiting for the timer.
func TestFileTailer_FullWindowCyclesImmediately(t *testing.T) {
	shipperEnv(t, t.TempDir(), shipperConfig{commitInterval: time.Hour, windowBytes: 2048, shutdownTimeout: time.Second})
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, true)
	defer stop()
	appendLog(t, log, lines("w", 1, 200))
	waitFor(t, "all lines", func() bool { return len(uniq(coord.raws())) == 200 })
	if n := coord.openCount(); n < 2 {
		t.Errorf("opens = %d; window-full did not trigger an ack cycle", n)
	}
}

// Quiet files hold no stream: cost scales with active files.
func TestFileTailer_QuietFileOpensNoStream(t *testing.T) {
	shipperEnv(t, t.TempDir(), fastCfg())
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, true)
	defer stop()
	time.Sleep(500 * time.Millisecond) // several commit intervals, no lines
	if n := coord.openCount(); n != 0 {
		t.Errorf("quiet file opened %d streams, want 0", n)
	}
}

// With checkpointing off (nil store) behaviour matches the old agent: one
// long-lived stream, no ack cycling.
func TestFileTailer_DisabledCheckpointingKeepsOneStream(t *testing.T) {
	shipperEnv(t, "", fastCfg())
	log := filepath.Join(t.TempDir(), "a.log")
	appendLog(t, log, "")
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, true)
	defer stop()
	for i := 0; i < 5; i++ {
		appendLog(t, log, lines("d", i*5+1, i*5+5))
		time.Sleep(150 * time.Millisecond) // > commitInterval between batches
	}
	waitFor(t, "lines", func() bool { return len(uniq(coord.raws())) == 25 })
	if n := coord.openCount(); n != 1 {
		t.Errorf("opens = %d, want 1 (no cycling without checkpoints)", n)
	}
}

// Files created while weftd runs are read from their first byte.
func TestFileTailer_RuntimeDiscoveredFileReadsFromStart(t *testing.T) {
	shipperEnv(t, t.TempDir(), fastCfg())
	log := filepath.Join(t.TempDir(), "new.log")
	appendLog(t, log, lines("first", 1, 3)) // written before we open it
	coord := &fakeCoord{}

	stop := runAgent(t, coord, log, false)
	defer stop()
	waitFor(t, "initial content", func() bool { return len(uniq(coord.raws())) == 3 })
}
