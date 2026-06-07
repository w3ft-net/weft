package coordinator

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	"time"
)

// This file documents the PRE-PLAN splicer path specifically: when
// a /bin script's body is a single pipeline made of simple commands,
// expandBinPipeline inlines the body into the caller's pipeline so
// the planner sees one unified shape and can push predicates into
// shard SQL. The test below is a guardrail for that splice.
//
// The end-to-end behaviour of multi-list / loop / cmdsub / bodies
// (which the splicer deliberately skips — see bin_expand.go — and
// which run through the runtime PipelineInterceptor instead) is
// covered by coordinator/bin_interior_query_test.go. Those tests
// exercise the full coord.Execute path with real shards and assert on
// the stitched-together stdout, not planner output.

func planAfterExpand(t *testing.T, fs *testFS, pipeline string) *planner.QueryPlan {
	t.Helper()
	expanded, err := expandBinPipeline(pipeline, fs)
	if err != nil {
		t.Fatalf("expandBinPipeline: %v", err)
	}
	plan, err := planner.Plan(expanded, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("planner.Plan(%q): %v", expanded, err)
	}
	return plan
}

// Single-pipeline, simple-command body: the splicer inlines it and
// the planner emits ShardSQL carrying the interior predicate. If this
// breaks, the splicer has regressed — end-to-end tests may still
// pass (via the interceptor fallback) but the pushed-down form will
// be less efficient.
func TestBinPushdown_SinglePipelineBody_PushesDown(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/errors", `cat /log/$1 | grep ERROR`)

	plan := planAfterExpand(t, fs, "errors api.log")

	if plan.ShardSQL == "" {
		t.Fatalf("single-pipeline body should produce ShardSQL, got empty (plan=%+v)", plan)
	}
	if !strings.Contains(plan.ShardSQL, "ERROR") {
		t.Errorf("ShardSQL should carry the grep ERROR predicate, got %q", plan.ShardSQL)
	}
}
