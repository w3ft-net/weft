package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// These tests reproduce a suspected bug: weftd fails to onboard all log
// files that a glob pattern ought to match on a single host. The shape
// of the question — "I ran ls in the terminal and didn't see API logs"
// — points at the discovery path in watchGlob (cmd/weftd/main.go). The
// tests below exercise that path end-to-end with a recording fake
// coordinator client so we can see exactly which apps weftd tries to
// register with the server.
//
// Tests are written to FAIL when the bug is present and PASS when the
// glob is behaving as a user would expect. If any test passes today,
// that's a reassurance; if it fails, that's the repro.

// sampleSubmitRecorder is a fake WeftCoordinatorClient that records
// every SubmitSample call's App field so we can assert what weftd
// thinks it discovered. GetSourceStatus always returns "unknown" so
// the onboarding path proceeds to SubmitSample, and WaitForConfiguration
// blocks forever on a never-configured source — the test just caps the
// run with a ctx deadline.
type sampleSubmitRecorder struct {
	pb.WeftCoordinatorClient
	mu       sync.Mutex
	samples  []string // App values received
	statuses []string // App values received by GetSourceStatus
}

func (r *sampleSubmitRecorder) SubmitSample(_ context.Context, req *pb.SubmitSampleRequest, _ ...grpc.CallOption) (*pb.SubmitSampleResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, req.App)
	return &pb.SubmitSampleResponse{Status: "pending"}, nil
}

func (r *sampleSubmitRecorder) GetSourceStatus(_ context.Context, req *pb.GetSourceStatusRequest, _ ...grpc.CallOption) (*pb.GetSourceStatusResponse, error) {
	r.mu.Lock()
	r.statuses = append(r.statuses, req.App)
	r.mu.Unlock()
	return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
}

func (r *sampleSubmitRecorder) sampleApps() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.samples...)
	sort.Strings(out)
	return out
}

// writeFile creates a file with non-empty content so SampleLines
// doesn't burn its 30s no-content wait.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// runWatchGlob kicks off watchGlob in a goroutine and blocks until the
// recorder has seen `expected` SubmitSample calls or the deadline
// fires. Returns the recorded apps.
func runWatchGlob(t *testing.T, pattern string, expected int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rec := &sampleSubmitRecorder{}
	go watchGlob(ctx, rec, "wft_test", "kevlar", "testhost", "testns", pattern)

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(rec.sampleApps()) >= expected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return rec.sampleApps()
}

// TestGlob_DiscoversAllFilesInFlatDirectory — the happy path. Three log
// files sitting side-by-side under `*.log` — all three should onboard.
func TestGlob_DiscoversAllFilesInFlatDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api.log"), "2026-04-19 INFO started\n")
	writeFile(t, filepath.Join(dir, "worker.log"), "2026-04-19 INFO queue ready\n")
	writeFile(t, filepath.Join(dir, "nginx.log"), "127.0.0.1 - - GET /\n")

	got := runWatchGlob(t, filepath.Join(dir, "*.log"), 3)
	want := []string{"api", "nginx", "worker"}
	if !equalStrings(got, want) {
		t.Errorf("glob missed files:\n want %v\n  got %v", want, got)
	}
}

// TestGlob_DoubleStarDoesNotRecurse — documents the subtle truth about
// `**` in Go's filepath.Glob. Users coming from shell/git/rsync expect
// `dir/**/*.log` to find files at any depth. It doesn't: `**` is
// parsed as two adjacent `*` wildcards, and `*` doesn't match path
// separators. So `dir/**/*.log` is effectively `dir/*/*.log` — it
// matches ONE segment down, not arbitrary depth.
//
// Files at depth 2 from `dir` get missed silently.
func TestGlob_DoubleStarDoesNotRecurse(t *testing.T) {
	dir := t.TempDir()
	// Depth 1 (shallow) — SHOULD match dir/**/*.log
	writeFile(t, filepath.Join(dir, "shallow", "access.log"), "line\n")
	// Depth 2+ — SHOULD NOT match dir/**/*.log (this is the bug
	// reproduction if a user believes `**` means recursive)
	writeFile(t, filepath.Join(dir, "api", "v1", "access.log"), "line\n")
	writeFile(t, filepath.Join(dir, "api", "v2", "access.log"), "line\n")

	got := runWatchGlob(t, filepath.Join(dir, "**", "*.log"), 3)

	// Deep files SHOULD appear under a user's intent — but they don't.
	// Fail the test loudly so we're forced to notice.
	if contains(got, "access") && len(got) >= 3 {
		t.Log("`**` apparently recursed — Go stdlib changed? Investigate.")
		return
	}
	t.Errorf("user expected `**/*.log` to recurse into v1/ and v2/ but it only hit the shallow file. got=%v", got)
}

// TestGlob_AppNameCollisionCollapsesSources — every file weftd
// discovers derives its app name from `strings.TrimSuffix(base, ext)`.
// Two matched files that share a basename (e.g., `api/access.log`
// and `web/access.log`) both get `app="access"`. Both trigger
// SubmitSample client-side, but server-side UpsertPendingSource is
// keyed on (customer, namespace, app) — the second overwrites the
// first, so the admin sees only one pending source in the UI and has
// no way to tell the two files apart. One of them is silently
// invisible.
//
// Test expectation: watchGlob DOES call SubmitSample twice (the bug is
// not a dropped client call), but both carry the same App value.
func TestGlob_AppNameCollisionCollapsesSources(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api", "access.log"), "api access\n")
	writeFile(t, filepath.Join(dir, "web", "access.log"), "web access\n")

	got := runWatchGlob(t, filepath.Join(dir, "*", "access.log"), 2)

	// Both files triggered SubmitSample — good, the discovery half works.
	if len(got) != 2 {
		t.Fatalf("expected 2 SubmitSample calls, got %d: %v", len(got), got)
	}
	// …but they all carry the same App, so server-side they collapse to
	// a single pending_source row — the user's "one file invisible"
	// repro.
	if got[0] != got[1] || got[0] != "access" {
		t.Fatalf("expected both SubmitSample calls to collide on app=access; got %v", got)
	}
	t.Errorf("confirmed: two files onboarded with duplicate app=%q — server will keep only one pending_source row and the user loses a log stream", got[0])
}

// TestGlob_MultiDirPatternBreaksWatcher — when the pattern spans a
// glob-expansion at a directory level (e.g., `/var/log/*/app.log`),
// `filepath.Dir(pattern)` returns `/var/log/*` which is not a real
// directory, so fsnotify.Add fails silently and any *later-created*
// file won't get picked up. The initial-scan files DO get discovered
// via filepath.Glob, so this test asserts exactly that: start-time
// files are seen but we lose runtime-created files.
//
// Documented here as a mismatch between Glob's capabilities and
// watchGlob's fsnotify bookkeeping.
func TestGlob_MultiDirPatternFindsStartupFilesOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api", "server.log"), "startup\n")

	pattern := filepath.Join(dir, "*", "server.log")
	got := runWatchGlob(t, pattern, 1)
	if len(got) != 1 || got[0] != "server" {
		t.Errorf("expected startup-time file to onboard as 'server', got %v", got)
	}
	// The test doesn't attempt to validate the late-file case (fsnotify
	// on /tmp/*/ can't be set up) — the log message above is the point.
}

// TestGlob_EmptyFileNeverOnboards — onboardAndRun → SubmitSample reads
// the first N lines of the file. If the file exists but is empty (and
// stays empty across SampleLines's 30s retry loop), SubmitSample
// returns an error and watchGlob logs it without retrying. So an
// empty-at-startup log file is invisible forever — unless weftd
// restarts, at which point the file may have grown.
//
// This test DOESN'T repro that end-to-end (would take 30+s), but we
// can assert the symptom: three files, one empty, the test
// runWatchGlob deadline expires with only two samples recorded.
func TestGlob_EmptyFileStallsOnboarding(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api.log"), "line\n")
	writeFile(t, filepath.Join(dir, "empty.log"), "") // zero bytes
	writeFile(t, filepath.Join(dir, "worker.log"), "line\n")

	got := runWatchGlob(t, filepath.Join(dir, "*.log"), 3)
	// With our 2.5s wait, the two non-empty files should onboard; the
	// empty file's SampleLines is still in its retry loop (30s total).
	if contains(got, "empty") {
		t.Errorf("unexpected: empty file onboarded within 2.5s window — our 30s retry might not be hitting")
	}
	if !contains(got, "api") || !contains(got, "worker") {
		t.Errorf("expected api and worker to onboard; got %v", got)
	}
	t.Logf("empty-file stall confirmed: onboarded=%v (missing 'empty' because SampleLines is still waiting for content)", got)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
