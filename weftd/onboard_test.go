package weftd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestSampleLinesAccumulatesToTarget pins the "1 line each" bug: a slow
// source has only a single line present when sampling starts, then more
// lines trickle in. The sampler must wait and accumulate up to the target
// rather than returning the first line it sees.
func TestSampleLinesAccumulatesToTarget(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "xylem.log")
	if err := os.WriteFile(logPath, []byte("line 1\n"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	// A slow producer appends four more lines after sampling has begun.
	go func() {
		for i := 2; i <= 5; i++ {
			time.Sleep(100 * time.Millisecond)
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			f.WriteString("line " + string(rune('0'+i)) + "\n")
			f.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lines, err := SampleLines(ctx, logPath, 5, false, nil)
	if err != nil {
		t.Fatalf("SampleLines: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("expected sampler to accumulate 5 lines, got %d: %v", len(lines), lines)
	}
}

// TestSampleUnitLines verifies journald (unit:) sources are sampled from
// the unit's journal, not by opening a file path. Regression for the bug
// where onboardAndRun fed an empty src.Path into the file sampler and
// failed with `open "": no such file or directory`, so weftd could never
// onboard a journald source.
func TestSampleUnitLines(t *testing.T) {
	dir := t.TempDir()
	// Fake journalctl: emits three lines, ignores its args.
	fake := filepath.Join(dir, "journalctl")
	script := "#!/bin/sh\nprintf 'line one\\nline two\\nline three\\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake journalctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	lines, err := SampleUnitLines(context.Background(), "django.service", 50, false, nil)
	if err != nil {
		t.Fatalf("SampleUnitLines: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "line one" || lines[2] != "line three" {
		t.Fatalf("unexpected lines: %v", lines)
	}
}

// TestSampleUnitLinesCap honors the n cap.
func TestSampleUnitLinesCap(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "journalctl")
	script := "#!/bin/sh\nprintf 'a\\nb\\nc\\nd\\n'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake journalctl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	lines, err := SampleUnitLines(context.Background(), "x.service", 2, false, nil)
	if err != nil {
		t.Fatalf("SampleUnitLines: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected cap of 2, got %d: %v", len(lines), lines)
	}
}

// A fresh (re)sample must skip what is already in the file: the head holds
// the startup chatter whose absence of useful fields is why a resample was
// asked for in the first place.
func TestSampleLinesFreshSkipsExistingContent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logPath, []byte("boot 1\nboot 2\nboot 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		f.WriteString("req 1 trace_id=a\nreq 2 trace_id=b\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lines, err := SampleLines(ctx, logPath, 2, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "req 1 trace_id=a" || lines[1] != "req 2 trace_id=b" {
		t.Fatalf("fresh sample = %v, want only the lines written after sampling began", lines)
	}
}

// versionRecorder captures the request weftd sends when it checks a source.
type versionRecorder struct {
	pb.WeftCoordinatorClient
	got *pb.GetSourceStatusRequest
}

func (r *versionRecorder) GetSourceStatus(_ context.Context, req *pb.GetSourceStatusRequest, _ ...grpc.CallOption) (*pb.GetSourceStatusResponse, error) {
	r.got = req
	return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
}

// The agent says what it is every time it polls a source, so the server can
// tell which agents are reading a schema.
func TestCheckSourceStatusReportsAgentVersionAndHost(t *testing.T) {
	AgentVersion, AgentInstance = "0.28.0", "web-1"
	defer func() { AgentVersion, AgentInstance = "", "" }()
	rec := &versionRecorder{}
	if _, err := CheckSourceStatus(context.Background(), rec, "wft_x", "api"); err != nil {
		t.Fatal(err)
	}
	if rec.got.AgentVersion != "0.28.0" || rec.got.Instance != "web-1" || rec.got.Service != "api" {
		t.Errorf("request = %+v, want the agent's version and host alongside the service", rec.got)
	}
}
