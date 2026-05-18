package weftd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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

	lines, err := SampleUnitLines(context.Background(), "django.service", 50)
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

	lines, err := SampleUnitLines(context.Background(), "x.service", 2)
	if err != nil {
		t.Fatalf("SampleUnitLines: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected cap of 2, got %d: %v", len(lines), lines)
	}
}
