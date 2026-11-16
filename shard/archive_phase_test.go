package shard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

func archiveFixtureShard(t *testing.T) *DuckDBShard {
	t.Helper()
	s, err := NewDuckDBShard("phase-test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	base := int64(1_700_000_000) * int64(time.Second)
	rows := []*pb.LogRow{
		{Raw: "a1", CustomerId: 5, TsUnixNs: base},
		{Raw: "a2", CustomerId: 5, TsUnixNs: base + int64(time.Hour)},
		{Raw: "g1", CustomerId: 6, TsUnixNs: base + int64(30*time.Minute)},
	}
	if err := s.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return s
}

// Archive must honor its context: the coordinator abandons an attempt at
// archiveTimeout, and an export that keeps running after that piles up
// against the next attempt.
func TestDuckDBShard_ArchiveHonorsCancelledContext(t *testing.T) {
	s := archiveFixtureShard(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.Archive(ctx, t.TempDir())
	if err == nil {
		t.Fatal("Archive with a cancelled context returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	// The failed attempt must release the in-flight guard.
	if _, _, err := s.Archive(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Archive after a cancelled attempt: %v", err)
	}
}

// A second Archive while one is running must refuse rather than pile on,
// and say how long the first has been going.
func TestDuckDBShard_ArchiveRejectsOverlap(t *testing.T) {
	s := archiveFixtureShard(t)
	_, end, err := s.beginArchive()
	if err != nil {
		t.Fatalf("first beginArchive: %v", err)
	}

	_, _, err = s.Archive(context.Background(), t.TempDir())
	if !errors.Is(err, ErrArchiveInFlight) {
		t.Fatalf("overlapping Archive err = %v, want ErrArchiveInFlight", err)
	}
	if !strings.Contains(err.Error(), "attempt 1") || !strings.Contains(err.Error(), "s in") {
		t.Fatalf("overlap error should name the running attempt and its age: %v", err)
	}

	end()
	if _, _, err := s.Archive(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Archive after the first attempt ended: %v", err)
	}
}

// Every attempt logs its phases so a stuck export can be diagnosed from the
// shard's own log: attempt id, stats scan, sizing decision, copy, result.
func TestDuckDBShard_ArchiveLogsPhases(t *testing.T) {
	s := archiveFixtureShard(t)
	var archiveErr error
	out := captureLog(t, func() {
		_, _, archiveErr = s.Archive(context.Background(), t.TempDir())
	})
	if archiveErr != nil {
		t.Fatalf("Archive: %v", archiveErr)
	}
	for _, want := range []string{
		"archive attempt=1 start",
		"archive attempt=1 stats partitions=2",
		"archive attempt=1 sizing",
		"bucket_minutes=",
		"archive attempt=1 copy start",
		"archive attempt=1 copy done rows=3",
		"archive attempt=1 done",
		"mem rss_kb=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

func TestDuckDBShard_ArchiveLogsFailure(t *testing.T) {
	s := archiveFixtureShard(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := captureLog(t, func() { _, _, _ = s.Archive(ctx, t.TempDir()) })
	if !strings.Contains(out, "archive attempt=1 failed") {
		t.Fatalf("failure not logged:\n%s", out)
	}
}

// The thrash happens mid-COPY, so memory must be sampled while the export
// runs, not only at phase boundaries.
func TestDuckDBShard_SampleArchiveMemoryLogsWhileRunning(t *testing.T) {
	s := archiveFixtureShard(t)
	out := captureLog(t, func() {
		stop := s.sampleArchiveMemory(1, "copy", 5*time.Millisecond)
		time.Sleep(60 * time.Millisecond)
		stop()
	})
	if n := strings.Count(out, "archive attempt=1 progress phase=copy"); n < 2 {
		t.Fatalf("got %d progress samples, want >=2:\n%s", n, out)
	}
	if !strings.Contains(out, "mem rss_kb=") {
		t.Fatalf("samples carry no memory reading:\n%s", out)
	}

	// After stop() returns, nothing more is logged.
	after := captureLog(t, func() { time.Sleep(30 * time.Millisecond) })
	if after != "" {
		t.Fatalf("sampler kept logging after stop: %q", after)
	}
}

// The S3 path pins the pool to one connection (S3Config.ConfigureDuckDB), and
// the COPY holds it for the whole export. Sampling must not need a pooled
// connection, or no reading would ever print while the export runs.
func TestDuckDBShard_SampleArchiveMemoryDoesNotNeedAPooledConnection(t *testing.T) {
	s := archiveFixtureShard(t)
	s.db.SetMaxOpenConns(1)
	held, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	var out string
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		out = captureLog(t, func() {
			stop := s.sampleArchiveMemory(1, "copy", 5*time.Millisecond)
			time.Sleep(60 * time.Millisecond)
			stop()
		})
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("sampler blocked waiting for the held connection")
	}
	if n := strings.Count(out, "archive attempt=1 progress"); n < 2 {
		t.Fatalf("got %d samples while the connection was held, want >=2:\n%s", n, out)
	}
}

const procStatusFixture = `Name:	weft-shard
VmPeak:	  912340 kB
VmRSS:	  702112 kB
VmSwap:	  524288 kB
Threads:	14
`

func TestParseProcStatus(t *testing.T) {
	rss, swap := parseProcStatusKB(procStatusFixture)
	if rss != 702112 || swap != 524288 {
		t.Fatalf("rss=%d swap=%d, want 702112/524288", rss, swap)
	}
	if rss, swap := parseProcStatusKB(""); rss != 0 || swap != 0 {
		t.Fatalf("empty input: rss=%d swap=%d", rss, swap)
	}
}

const psiFixture = `some avg10=12.50 avg60=3.10 avg300=0.80 total=123456
full avg10=7.25 avg60=1.00 avg300=0.10 total=65432
`

func TestParsePSI(t *testing.T) {
	some, full := parsePSIAvg10(psiFixture)
	if some != 12.50 || full != 7.25 {
		t.Fatalf("some=%v full=%v, want 12.5/7.25", some, full)
	}
	if some, full := parsePSIAvg10(""); some != 0 || full != 0 {
		t.Fatalf("empty input: some=%v full=%v", some, full)
	}
}

// The DuckDB-native readings must work against a real handle; /proc readings
// are best-effort (absent off Linux) and must never fail the diagnostic.
func TestDuckDBShard_NativeMemoryDiag(t *testing.T) {
	s := archiveFixtureShard(t)
	d := s.nativeMemoryDiag(context.Background())
	if d.MemoryLimit == "" {
		t.Errorf("memory_limit not read: %+v", d)
	}
	if d.Threads <= 0 {
		t.Errorf("threads not read: %+v", d)
	}
	if !strings.Contains(d.String(), "mem rss_kb=") {
		t.Errorf("String() = %q", d.String())
	}
}
