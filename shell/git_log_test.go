package shell

import (
	"bytes"
	"testing"
)

// TestExecNonInteractiveGitLogShipsVerbatim guards that `git log
// /bin/scan` reaches the server unmangled. Historically the client's
// RewritePaths could mangle `git log ...` into `git /log ...` (joining
// `log` to the `/log` namespace root); now ExecNonInteractive ships the
// line verbatim and the server owns all path resolution, so the git
// subcommand keyword is preserved by construction.
func TestExecNonInteractiveGitLogShipsVerbatim(t *testing.T) {
	var captured string
	s := &Shell{
		Range: "1d..",
		Query: func(pipeline, _ string, _ []string) (*QueryResult, error) {
			captured = pipeline
			return &QueryResult{Text: ""}, nil
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("git log /bin/scan", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if captured != "git log /bin/scan" {
		t.Errorf("Query received %q, want %q shipped verbatim", captured, "git log /bin/scan")
	}
}
