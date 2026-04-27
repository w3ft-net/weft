package shell

import (
	"bytes"
	"testing"
	"time"
)

// TestExecNonInteractiveWitLogPreservesSubcommand is the round-trip
// for the wit-log-rewriter plan. Before the grammar registration
// landed in shell.go, `weft sh -c 'wit log /bin/scan'` got mangled
// to `wit /log /bin/scan` by RewritePaths because `log` joined to
// `/` resolves to the `/log` namespace root (PathQuery). The test
// asserts the rewriter now preserves `log` as wit's subcommand
// keyword by checking what pipeline reaches the Query callback.
func TestExecNonInteractiveWitLogPreservesSubcommand(t *testing.T) {
	var captured string
	s := &Shell{
		Query: func(pipeline string, start, end time.Time) (*QueryResult, error) {
			captured = pipeline
			return &QueryResult{Text: ""}, nil
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("wit log /bin/scan", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	if captured != "wit log /bin/scan" {
		t.Errorf("Query received %q, want %q — rewriter mangled the wit subcommand keyword", captured, "wit log /bin/scan")
	}
}
