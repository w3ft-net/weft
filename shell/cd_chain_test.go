package shell

import (
	"bytes"
	"strings"
	"testing"
)

// These tests codify the examples in docs/plans/cd-cwd-state-hook.md
// for the non-interactive (`weft sh -c`) path. The DirStack +
// RewritePaths plumbing is supposed to make `cd <dir> && <cmd>`,
// `cd <dir>; <cmd>`, and pushd/popd chains carry their cwd state
// across to the source command before the planner sees the
// pipeline. Each test stubs Shell.Query and asserts on the
// pipeline string the shell hands to the planner.
//
// The test stubs short-circuit the rewrite: whatever the captured
// `pipeline` is, that's what the coordinator/planner sees. So the
// assertion is simply "did the rewriter substitute api.log →
// /log/acme/api.log on the source command?"

func newCaptureShell() (*Shell, *string) {
	var captured string
	s := &Shell{
		FS: testMountFS(nil),
		Query: func(pipeline, _ string) (*QueryResult, error) {
			captured = pipeline
			return &QueryResult{Text: ""}, nil
		},
	}
	return s, &captured
}

// TestExecNonInteractiveCdAndCat is the headline example from the
// plan doc:
//
//	weft sh -c "cd /log/acme && cat api.log | head -1"
//
// The rewriter should fold the cd into a transient DirStack and
// rewrite api.log against /log/acme before the planner sees it.
func TestExecNonInteractiveCdAndCat(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme && cat api.log | head -1", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if !strings.Contains(*captured, "cat /log/acme/api.log") {
		t.Errorf("Query received %q, want it to contain cat /log/acme/api.log — cd && was not folded into the cat arg", *captured)
	}
}

// TestExecNonInteractiveCdSemicolon: the doc's semicolon form
// (TestRewritePathsSemicolon at the rewriter level). End-to-end
// through ExecNonInteractive, the captured pipeline must carry
// the rewritten cat arg.
func TestExecNonInteractiveCdSemicolon(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme; cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if !strings.Contains(*captured, "cat /log/acme/api.log") {
		t.Errorf("Query received %q, want it to contain cat /log/acme/api.log", *captured)
	}
}

// TestExecNonInteractiveRelativeCdChain: stacked relative cds.
// Mirrors TestRewriteRelativeCdChain in warp's bash package — the
// transient DirStack should accumulate /log → /log/acme as the
// rewriter walks the &&-chain.
func TestExecNonInteractiveRelativeCdChain(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log && cd acme && cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if !strings.Contains(*captured, "cat /log/acme/api.log") {
		t.Errorf("Query received %q, want it to contain cat /log/acme/api.log", *captured)
	}
}

// TestExecNonInteractivePushdCat: pushd advances the transient
// DirStack the same way cd does. The cat after && should resolve
// against the pushed dir.
func TestExecNonInteractivePushdCat(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("pushd /log/acme && cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if !strings.Contains(*captured, "cat /log/acme/api.log") {
		t.Errorf("Query received %q, want it to contain cat /log/acme/api.log", *captured)
	}
}

// TestExecNonInteractivePushdPopdCat: pushd then popd should
// return the transient DirStack to /, so the trailing cat
// resolves against / (and 'foo' is not in /log/, so the rewriter
// leaves it alone — the cwd state has rolled back).
func TestExecNonInteractivePushdPopdCat(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("pushd /log/acme && cat api.log && popd && cat /log/other/x.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	// First cat after pushd: resolved against /log/acme.
	if !strings.Contains(*captured, "cat /log/acme/api.log") {
		t.Errorf("Query received %q, want first cat to resolve to /log/acme/api.log", *captured)
	}
	// Second cat after popd: absolute path, untouched.
	if !strings.Contains(*captured, "cat /log/other/x.log") {
		t.Errorf("Query received %q, want second cat to keep /log/other/x.log", *captured)
	}
}
