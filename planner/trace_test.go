package planner

import (
	"strings"
	"testing"
	"time"
)

// TestPlanTrace_Predicate: `trace <id>` emits a parameterized trace_id
// predicate and no namespace/service/kind predicate — it fans out across
// every source the customer can read, the same way `cat warp.log` does.
func TestPlanTrace_Predicate(t *testing.T) {
	plan, err := Plan(`trace abc-123`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "trace_id = ?") {
		t.Errorf("ShardSQL missing trace_id predicate: %s", plan.ShardSQL)
	}
	found := false
	for _, a := range plan.Args {
		if a == "abc-123" {
			found = true
		}
	}
	if !found {
		t.Errorf("Args missing trace id %q: %v", "abc-123", plan.Args)
	}
	for _, col := range []string{"namespace =", "service =", "kind =", "namespace IN", "service IN", "kind IN"} {
		if strings.Contains(plan.ShardSQL, col) {
			t.Errorf("ShardSQL unexpectedly scoped by %q (trace should fan out across all sources): %s", col, plan.ShardSQL)
		}
	}
}

// TestPlanTrace_OrderAndScope: trace inherits the same Mode A defaults as
// plain `cat` — shard-side ts ascending ordering — and SourceNamespace/
// SourceService stay empty so ShardDescriptorsFor(namespaceID=0) fans out
// across every app/service, the same live path warp.log already exercises.
func TestPlanTrace_OrderAndScope(t *testing.T) {
	plan, err := Plan(`trace abc-123`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.ShardOrderBy != "ts" {
		t.Errorf("ShardOrderBy: got %q, want %q", plan.ShardOrderBy, "ts")
	}
	if plan.SourceNamespace != "" {
		t.Errorf("SourceNamespace: got %q, want empty", plan.SourceNamespace)
	}
	if plan.SourceService != "" {
		t.Errorf("SourceService: got %q, want empty", plan.SourceService)
	}
}

// TestPlanTrace_TimeBoundsApplied: trace still respects the shell's
// default/explicit time range, same as cat/head/tail — sql.addTimeBounds
// runs unconditionally before source-command dispatch.
func TestPlanTrace_TimeBoundsApplied(t *testing.T) {
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	plan, err := Plan(`trace abc-123`, "kevlar", start, end)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "ts >= CAST(? AS TIMESTAMP)") {
		t.Errorf("missing lower time bound: %s", plan.ShardSQL)
	}
	if !strings.Contains(plan.ShardSQL, "ts <= CAST(? AS TIMESTAMP)") {
		t.Errorf("missing upper time bound: %s", plan.ShardSQL)
	}
}

// TestPlanTrace_IncludeMetaVars: a bare trace forces the same
// MetaVarColumns projection normally reserved for an explicit awk/jq
// meta-variable reference, so NAMESPACE/SERVICE/etc. are always real,
// pipeline-filterable fields for trace (and so compute/local.go's
// display layer has namespace/service available on every row) even
// with no downstream compute stage.
func TestPlanTrace_IncludeMetaVars(t *testing.T) {
	plan, err := Plan(`trace abc-123`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, col := range MetaVarColumns {
		if !strings.Contains(plan.ShardSQL, ", "+col) {
			t.Errorf("ShardSQL missing MetaVarColumns projection %q: %s", col, plan.ShardSQL)
		}
	}
}

// TestPlanTrace_ArityErrors: trace requires exactly one trace id argument.
func TestPlanTrace_ArityErrors(t *testing.T) {
	for _, pipeline := range []string{"trace", "trace id1 id2"} {
		t.Run(pipeline, func(t *testing.T) {
			if _, err := Plan(pipeline, "kevlar", time.Time{}, time.Time{}); err == nil {
				t.Errorf("Plan(%q): expected error, got nil", pipeline)
			}
		})
	}
}
