package shell

import (
	"bytes"
	"testing"
)

// TestExecNonInteractiveWitLogShipsVerbatim guards that `wit log
// /bin/scan` reaches the server unmangled. Historically the client's
// RewritePaths could mangle `wit log ...` into `wit /log ...` (joining
// `log` to the `/log` namespace root); now ExecNonInteractive ships the
// line verbatim and the server owns all path resolution, so the wit
// subcommand keyword is preserved by construction.
func TestExecNonInteractiveWitLogShipsVerbatim(t *testing.T) {
	var captured string
	s := &Shell{
		Range: "1d..",
		Query: func(pipeline, _ string, _ []string) (*QueryResult, error) {
			captured = pipeline
			return &QueryResult{Text: ""}, nil
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("wit log /bin/scan", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if captured != "wit log /bin/scan" {
		t.Errorf("Query received %q, want %q shipped verbatim", captured, "wit log /bin/scan")
	}
}
