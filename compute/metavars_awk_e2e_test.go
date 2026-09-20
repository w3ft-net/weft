package compute

import (
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestLocalComputeJob_AwkMetaVarOutputRead is the end-to-end regression
// test for awk's half of "reserved meta-variables ... resolve
// incorrectly when read as output" (docs/known-issues.md, now fixed):
// runs a real awk stage through warp's bash.RunStreamingWithAwkHook
// (not just awkVarsFromMeta in isolation) and asserts a bare HOST read
// resolves to the row's actual instance column.
func TestLocalComputeJob_AwkMetaVarOutputRead(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{`awk '{ print HOST }'`}, 1, "ts", 0, false)
	job.SendRow(&pb.ResultRow{
		TsUnixNs: 1000,
		Raw:      "some log line",
		MetaVars: map[string]string{"instance": "web-1"},
	}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "web-1" {
		t.Errorf("awk '{ print HOST }' = %q, want %q", got, "web-1")
	}
}

// A row with no MetaVars (the overwhelming common case — most queries
// have no awk stage reading a meta-variable) must pass through
// unmodified: HOST is genuinely unset, so it reads as awk's ordinary
// uninitialized value (empty string) — unlike the old "$HOST" syntax,
// which evaluated to $0 (the whole record) instead of empty (see
// docs/known-issues.md's fixed entry). This pins that the syntax
// migration actually fixed the unset case's failure mode too, not
// just the meta-var-set case.
func TestLocalComputeJob_AwkMetaVarOutputRead_NoMetaVarsIsEmpty(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{`awk '{ print HOST }'`}, 1, "ts", 0, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "some log line"}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "" {
		t.Errorf("awk '{ print HOST }' with no MetaVars = %q, want empty", got)
	}
}

// A plain awk stage that never references a meta-variable must not pay
// for the hook machinery at all — pins that stagesHaveAwk gating
// doesn't change ordinary awk behavior.
func TestLocalComputeJob_AwkOrdinaryStageUnaffected(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob([]string{`awk '{ print $1 }'`}, 1, "ts", 0, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "one two three"}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "one" {
		t.Errorf("awk '{ print $1 }' = %q, want %q", got, "one")
	}
}
