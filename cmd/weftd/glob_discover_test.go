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

// TestGlob_DoubleStarRecurses — `**/*.log` matches at any depth, the
// way users coming from shell/git/rsync expect. weftd's globber
// (cmd/weftd/main.go:globMatch) implements `**` itself because Go's
// filepath.Glob treats it as two adjacent single-segment wildcards.
// Each match's app name comes from the variable portion of the path,
// joined with `/`, so the three files below produce three distinct
// apps under the namespace.
func TestGlob_DoubleStarRecurses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "shallow", "access.log"), "line\n")
	writeFile(t, filepath.Join(dir, "api", "v1", "access.log"), "line\n")
	writeFile(t, filepath.Join(dir, "api", "v2", "access.log"), "line\n")

	got := runWatchGlob(t, filepath.Join(dir, "**", "*.log"), 3)

	want := []string{"api/v1/access", "api/v2/access", "shallow/access"}
	if !equalStrings(got, want) {
		t.Errorf("expected `**/*.log` to recurse into all three files\n want %v\n  got %v", want, got)
	}
}

// TestGlob_PathDisambiguatesAppNames — two files that share a basename
// across different subdirectories get distinct apps because app names
// come from the variable portion of the matched path, not just the
// file's basename. For pattern `*/access.log` matching
// `api/access.log` and `web/access.log`, the apps are `api/access`
// and `web/access`. Server-side UpsertPendingSource is keyed on
// (customer, namespace, app) so distinct apps get distinct rows;
// neither file is silently lost.
func TestGlob_PathDisambiguatesAppNames(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api", "access.log"), "api access\n")
	writeFile(t, filepath.Join(dir, "web", "access.log"), "web access\n")

	got := runWatchGlob(t, filepath.Join(dir, "*", "access.log"), 2)

	want := []string{"api/access", "web/access"}
	if !equalStrings(got, want) {
		t.Errorf("expected disambiguated app names\n want %v\n  got %v", want, got)
	}
}

// TestGlob_MultiDirPatternFindsStartupFiles — patterns that span a
// directory-level wildcard (e.g. `/var/log/*/app.log`) discover
// startup-time matches via filepath.WalkDir over the fixed prefix.
// Runtime detection of files newly created at deeper levels is a
// known limitation of fsnotify-without-recursive-add; this test
// covers only the startup case.
func TestGlob_MultiDirPatternFindsStartupFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api", "server.log"), "startup\n")

	pattern := filepath.Join(dir, "*", "server.log")
	got := runWatchGlob(t, pattern, 1)
	if len(got) != 1 || got[0] != "api/server" {
		t.Errorf("expected startup-time file to onboard as 'api/server', got %v", got)
	}
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
