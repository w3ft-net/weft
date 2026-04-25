package weftd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVersionReader_InitialRead verifies the reader picks up the file
// contents synchronously when the file exists at construction time.
func TestVersionReader_InitialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(path, []byte("v1.2.3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := NewVersionReader(context.Background(), path)
	if got := r.Current(); got != "v1.2.3" {
		t.Fatalf("Current() = %q, want %q", got, "v1.2.3")
	}
}

// TestVersionReader_LiveRefresh verifies the watcher picks up a
// rewritten file without an explicit refresh call.
func TestVersionReader_LiveRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "VERSION")
	if err := os.WriteFile(path, []byte("v1.0.0"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := NewVersionReader(ctx, path)
	if got := r.Current(); got != "v1.0.0" {
		t.Fatalf("initial Current() = %q, want v1.0.0", got)
	}

	// Rewrite. Use atomic-rename pattern (write to tmp, rename) to
	// match how a real deploy script would touch the file.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("v2.0.0"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Current() == "v2.0.0" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after rename, Current() = %q, want v2.0.0", r.Current())
}

// TestVersionReader_EmptyPath verifies empty path → empty value forever.
func TestVersionReader_EmptyPath(t *testing.T) {
	r := NewVersionReader(context.Background(), "")
	if got := r.Current(); got != "" {
		t.Fatalf("empty-path Current() = %q, want empty", got)
	}
}

// TestVersionReader_MissingFile verifies the reader gracefully tolerates
// a path that doesn't exist yet — Current() returns "" until the file
// shows up.
func TestVersionReader_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "VERSION")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := NewVersionReader(ctx, path)
	if got := r.Current(); got != "" {
		t.Fatalf("missing-file Current() = %q, want empty", got)
	}

	// Create the file mid-flight; reader should pick it up.
	if err := os.WriteFile(path, []byte("v0.0.1"), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.Current() == "v0.0.1" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after create, Current() = %q, want v0.0.1", r.Current())
}
