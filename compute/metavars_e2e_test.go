package compute

import (
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestJob_JqMetaVarOutputRead is the end-to-end regression
// test for "jq's reserved meta-variables ... resolve to null when read
// as output" (docs/known-issues.md): runs a real jq stage through
// warp's bash.RunStreaming (not just MergeMetaVars in isolation) and
// asserts a bare "$host" read resolves to the row's actual instance
// column instead of null.
func TestJob_JqMetaVarOutputRead(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`jq -r '$host'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{
		TsUnixNs: 1000,
		Raw:      `{"pct":42}`,
		MetaVars: map[string]string{"instance": "web-1"},
	}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "web-1" {
		t.Errorf("jq -r '$host' = %q, want %q", got, "web-1")
	}
}

// A row with no MetaVars (the overwhelming common case — most queries
// have no jq stage reading a meta-variable) must pass through
// unmodified: jq still runs, but "$host" is genuinely absent from the
// row, so it resolves to null exactly as it would against real
// standalone jq.
func TestJob_JqMetaVarOutputRead_NoMetaVarsIsNull(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`jq -r '$host'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: `{"pct":42}`}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "null" {
		t.Errorf("jq -r '$host' with no MetaVars = %q, want %q", got, "null")
	}
}

// A jq stage that names no meta-variable sees the row as it is: no
// "_"-prefixed keys, not even _ts, which every row carries. Otherwise
// `jq -c .` and `keys` would show them (docs/plans/planner-ir.md, "jq").
func TestJob_JqWithoutMetaVarSeesNoSplicedKeys(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`jq -c '.'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: `{"pct":42}`}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != `{"pct":42}` {
		t.Errorf("jq -c '.' = %q, want the row unchanged", got)
	}
}
