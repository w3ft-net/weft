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
	for _, pipeline := range []string{"trace", "trace id1 id2", "trace --spans"} {
		t.Run(pipeline, func(t *testing.T) {
			if _, err := Plan(pipeline, "kevlar", time.Time{}, time.Time{}); err == nil {
				t.Errorf("Plan(%q): expected error, got nil", pipeline)
			}
		})
	}
}

// TestPlanTrace_SpansFlag: `trace <id> --spans` plans identically to a
// bare `trace <id>` — the flag is stripped before the argument-count
// check and never reaches SQL. Position-independent (the flag can come
// before or after the id) because docs/plans/span-display.md's Phase 1
// rollup is entirely a client-side reinterpretation of the same rows.
func TestPlanTrace_SpansFlag(t *testing.T) {
	base, err := Plan(`trace abc-123`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan(bare): %v", err)
	}
	for _, pipeline := range []string{"trace abc-123 --spans", "trace --spans abc-123"} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, "kevlar", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", pipeline, err)
			}
			if plan.ShardSQL != base.ShardSQL {
				t.Errorf("ShardSQL differs from bare trace:\n  bare:  %s\n  spans: %s", base.ShardSQL, plan.ShardSQL)
			}
		})
	}
}

// TestPlanTraceSpans_Predicate: `trace spans` (Phase 2's cross-trace
// aggregation, docs/plans/span-display.md) filters to rows that carry
// a real duration_ns or are a "span error" line, with no trace_id or
// span_name predicate and no namespace/service/kind scoping — it fans
// out the same way a bare `trace <id>` does, just without a trace_id
// to anchor on.
func TestPlanTraceSpans_Predicate(t *testing.T) {
	plan, err := Plan(`trace spans`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "(duration_ns > 0 OR raw LIKE 'span error%')") {
		t.Errorf("ShardSQL missing the done/error row filter: %s", plan.ShardSQL)
	}
	if strings.Contains(plan.ShardSQL, "trace_id = ?") {
		t.Errorf("ShardSQL unexpectedly scoped by trace_id: %s", plan.ShardSQL)
	}
	if strings.Contains(plan.ShardSQL, "span_name = ?") {
		t.Errorf("ShardSQL unexpectedly scoped by span_name (no name given): %s", plan.ShardSQL)
	}
	for _, col := range []string{"namespace =", "service =", "kind =", "namespace IN", "service IN", "kind IN"} {
		if strings.Contains(plan.ShardSQL, col) {
			t.Errorf("ShardSQL unexpectedly scoped by %q: %s", col, plan.ShardSQL)
		}
	}
	if plan.SourceCmd != "trace_spans" {
		t.Errorf("SourceCmd: got %q, want trace_spans", plan.SourceCmd)
	}
}

// TestPlanTraceSpans_WithName: `trace spans <name>` adds a parameterized
// span_name predicate on top of the same done/error filter.
func TestPlanTraceSpans_WithName(t *testing.T) {
	plan, err := Plan(`trace spans shard.Query`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "span_name = ?") {
		t.Errorf("ShardSQL missing span_name predicate: %s", plan.ShardSQL)
	}
	found := false
	for _, a := range plan.Args {
		if a == "shard.Query" {
			found = true
		}
	}
	if !found {
		t.Errorf("Args missing span name %q: %v", "shard.Query", plan.Args)
	}
}

// TestPlanTraceSpans_IncludeMetaVars: same forced NAMESPACE/SERVICE
// projection as a bare trace — see TestPlanTrace_IncludeMetaVars.
func TestPlanTraceSpans_IncludeMetaVars(t *testing.T) {
	plan, err := Plan(`trace spans`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, col := range MetaVarColumns {
		if !strings.Contains(plan.ShardSQL, ", "+col) {
			t.Errorf("ShardSQL missing MetaVarColumns projection %q: %s", col, plan.ShardSQL)
		}
	}
}

// TestPlanTraceSpans_ArityErrors: at most one span name argument.
func TestPlanTraceSpans_ArityErrors(t *testing.T) {
	if _, err := Plan(`trace spans a b`, "kevlar", time.Time{}, time.Time{}); err == nil {
		t.Errorf("Plan(%q): expected error, got nil", "trace spans a b")
	}
}
