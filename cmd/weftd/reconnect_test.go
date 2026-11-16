package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/weftd"
	"google.golang.org/grpc"
)

// mockStream records sent messages and can be set to fail.
type mockStream struct {
	grpc.ClientStream
	mu       sync.Mutex
	sent     []*pb.LogRow
	failNext bool
}

func (m *mockStream) Send(row *pb.LogRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return fmt.Errorf("stream broken")
	}
	m.sent = append(m.sent, row)
	return nil
}

func (m *mockStream) CloseAndRecv() (*pb.IngestStreamResponse, error) {
	return &pb.IngestStreamResponse{}, nil
}

func (m *mockStream) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// mockCoordClient is a fake WeftCoordinatorClient that returns mockStreams.
type mockCoordClient struct {
	pb.WeftCoordinatorClient
	mu            sync.Mutex
	streams       []*mockStream
	streamIdx     int
	failOpenCount int32 // number of IngestStream calls to fail before succeeding
	openCalls     atomic.Int32
}

func (m *mockCoordClient) IngestStream(ctx context.Context, opts ...grpc.CallOption) (pb.WeftCoordinator_IngestStreamClient, error) {
	m.openCalls.Add(1)
	if remaining := atomic.AddInt32(&m.failOpenCount, -1); remaining >= 0 {
		return nil, fmt.Errorf("coordinator unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &mockStream{}
	m.streams = append(m.streams, s)
	return s, nil
}

func (m *mockCoordClient) latestStream() *mockStream {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streams) == 0 {
		return nil
	}
	return m.streams[len(m.streams)-1]
}

func (m *mockCoordClient) streamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// TestReconnect_SendRecovery verifies that sendLineWithReconnect opens a new
// stream when the current one breaks and successfully retries the send.
func TestReconnect_SendRecovery(t *testing.T) {
	client := &mockCoordClient{}
	ss := &streamState{}
	ctx := context.Background()

	// First send — should open a stream and send.
	if err := sendLineWithReconnect(ctx, client, ss, "line1", "i-001", "vimgym", ""); err != nil {
		t.Fatal(err)
	}
	if client.streamCount() != 1 {
		t.Fatalf("expected 1 stream opened, got %d", client.streamCount())
	}
	if client.latestStream().sentCount() != 1 {
		t.Fatalf("expected 1 message sent, got %d", client.latestStream().sentCount())
	}

	// Break the stream — next send should fail, trigger reconnect, and retry.
	client.latestStream().failNext = true

	if err := sendLineWithReconnect(ctx, client, ss, "line2", "i-001", "vimgym", ""); err != nil {
		t.Fatal(err)
	}
	if client.streamCount() != 2 {
		t.Fatalf("expected 2 streams (reconnect), got %d", client.streamCount())
	}
	if client.latestStream().sentCount() != 1 {
		t.Fatalf("expected 1 message on new stream, got %d", client.latestStream().sentCount())
	}
}

// TestReconnect_EmptyLinesSkipped verifies that blank lines don't trigger
// stream opens.
func TestReconnect_EmptyLinesSkipped(t *testing.T) {
	client := &mockCoordClient{}
	ss := &streamState{}
	ctx := context.Background()

	sendLineWithReconnect(ctx, client, ss, "", "i-001", "vimgym", "")
	sendLineWithReconnect(ctx, client, ss, "   ", "i-001", "vimgym", "")

	if client.streamCount() != 0 {
		t.Fatalf("expected no streams for empty lines, got %d", client.streamCount())
	}
}

// TestReconnect_OpenStreamRetries verifies that openStream retries with
// backoff when the coordinator is temporarily unavailable.
func TestReconnect_OpenStreamRetries(t *testing.T) {
	client := &mockCoordClient{}
	// Fail the first 2 IngestStream calls, succeed on the 3rd.
	atomic.StoreInt32(&client.failOpenCount, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := openStream(ctx, client, "test")
	if err != nil {
		t.Fatal(err)
	}
	if stream == nil {
		t.Fatal("expected non-nil stream")
	}
	if calls := client.openCalls.Load(); calls != 3 {
		t.Fatalf("expected 3 open attempts (2 failures + 1 success), got %d", calls)
	}
}

// TestReconnect_OpenStreamCancellation verifies that openStream respects
// context cancellation instead of retrying forever.
func TestReconnect_OpenStreamCancellation(t *testing.T) {
	client := &mockCoordClient{}
	// Fail indefinitely.
	atomic.StoreInt32(&client.failOpenCount, 1000)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := openStream(ctx, client, "test")
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

// TestStreamScannedLines_ReusesOneStream pins the weftd half of the
// coordinator leak: runJournalTailer used to pass a fresh &streamState{}
// per line, so a journald-sourced app opened one ingest stream per log
// line and abandoned each — churning streams and stranding a coordinator
// handler goroutine in Recv() per line. The journal path must reuse a
// single stream across all lines (like runFileTailer) and half-close it
// once at the end.
func TestStreamScannedLines_ReusesOneStream(t *testing.T) {
	client := &mockCoordClient{}
	const n = 50
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}

	streamScannedLines(context.Background(), client, strings.NewReader(sb.String()), "i-001", "xylem/django")

	if got := client.streamCount(); got != 1 {
		t.Fatalf("journal tailer opened %d streams for %d lines; want exactly 1 reused stream "+
			"(a fresh stream per line is the leak)", got, n)
	}
	if got := client.streams[0].sentCount(); got != n {
		t.Fatalf("reused stream carried %d rows; want all %d", got, n)
	}
}

// TestStreamScannedLines_LongLineDoesNotStopTheTailer pins the journal path
// against bufio.Scanner's 64 KiB token limit: a longer entry used to make
// Scan fail with ErrTooLong and the tailer exit silently, dropping every
// later line. Like file sources, an oversized line is cut at
// weftd.DefaultMaxLineBytes and marked, and the lines after it still ship.
func TestStreamScannedLines_LongLineDoesNotStopTheTailer(t *testing.T) {
	client := &mockCoordClient{}
	huge := strings.Repeat("x", weftd.DefaultMaxLineBytes+4096)
	in := "before\n" + huge + "\nafter\n"

	streamScannedLines(context.Background(), client, strings.NewReader(in), "i-001", "xylem/django")

	if client.streamCount() != 1 {
		t.Fatalf("opened %d streams; want 1", client.streamCount())
	}
	rows := client.streams[0].sent
	if len(rows) != 3 {
		t.Fatalf("shipped %d rows; want 3 (before, truncated, after)", len(rows))
	}
	if got := rows[0].Raw; got != "before" {
		t.Errorf("row 0 = %q", got)
	}
	if want := weftd.DefaultMaxLineBytes + len(weftd.TruncatedMarker); len(rows[1].Raw) != want ||
		!strings.HasSuffix(rows[1].Raw, weftd.TruncatedMarker) {
		t.Errorf("row 1 len %d, suffix ok=%v; want len %d ending in the truncation marker",
			len(rows[1].Raw), strings.HasSuffix(rows[1].Raw, weftd.TruncatedMarker), want)
	}
	if got := rows[2].Raw; got != "after" {
		t.Errorf("row 2 = %q", got)
	}
}
