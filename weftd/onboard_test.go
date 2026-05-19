package weftd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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

	lines, err := SampleLines(ctx, logPath, 5, nil)
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

	lines, err := SampleUnitLines(context.Background(), "django.service", 50, nil)
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

	lines, err := SampleUnitLines(context.Background(), "x.service", 2, nil)
	if err != nil {
		t.Fatalf("SampleUnitLines: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected cap of 2, got %d: %v", len(lines), lines)
	}
}
