package shell

import (
	"bytes"
	"testing"
)

// ExecNonInteractive (the `weft -c "..."` one-shot path) no longer
// rewrites cd/relative paths client-side — it ships the pipeline
// verbatim to the server, a real pkg/bash interpreter that owns
// cd/chain/relative-path resolution. These tests pin that the shell
// hands the line to Query unmangled; the *resolution* correctness
// (relative cat resolves against a chained cd) is verified server-side
// in the coordinator's dirstack courier tests (TestCourier_* in
// weft/coordinator) where a live interpreter actually runs the chain.

func newCaptureShell() (*Shell, *string) {
	var captured string
	s := &Shell{
		FS: testMountFS(nil),
		Query: func(pipeline, _ string, _ []string) (*QueryResult, error) {
			captured = pipeline
			return &QueryResult{Text: ""}, nil
		},
	}
	return s, &captured
}

// TestExecNonInteractiveCdAndCatShipsVerbatim: `cd X && cat Y | head`
// ships as exactly that — the server folds the cd and resolves Y.
func TestExecNonInteractiveCdAndCatShipsVerbatim(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme && cat api.log | head -1", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if *captured != "cd /log/acme && cat api.log | head -1" {
		t.Errorf("Query received %q, want the chain shipped verbatim (no client-side rewrite)", *captured)
	}
}

// TestExecNonInteractiveCdSemicolonShipsVerbatim: the `;` form ships
// unchanged too.
func TestExecNonInteractiveCdSemicolonShipsVerbatim(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log/acme; cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if *captured != "cd /log/acme; cat api.log" {
		t.Errorf("Query received %q, want it shipped verbatim", *captured)
	}
}

// TestExecNonInteractiveRelativeCdChainShipsVerbatim: stacked relative
// cds ship intact; the server accumulates them.
func TestExecNonInteractiveRelativeCdChainShipsVerbatim(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("cd /log && cd acme && cat api.log", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if *captured != "cd /log && cd acme && cat api.log" {
		t.Errorf("Query received %q, want it shipped verbatim", *captured)
	}
}

// TestExecNonInteractivePushdPopdShipsVerbatim: a pushd/popd chain ships
// whole — the server applies the stack ops and the trailing absolute cat.
func TestExecNonInteractivePushdPopdShipsVerbatim(t *testing.T) {
	s, captured := newCaptureShell()
	var buf bytes.Buffer
	const line = "pushd /log/acme && cat api.log && popd && cat /log/other/x.log"
	if err := s.ExecNonInteractive(line, &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if *captured != line {
		t.Errorf("Query received %q, want it shipped verbatim %q", *captured, line)
	}
}
