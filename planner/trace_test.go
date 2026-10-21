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
	plan, err := Plan(`trace --id abc-123`, 1, "", time.Time{}, time.Time{})
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
	plan, err := Plan(`trace --id abc-123`, 1, "", time.Time{}, time.Time{})
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
	plan, err := Plan(`trace --id abc-123`, 1, "", start, end)
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
// pipeline-filterable fields for trace (and so compute/engine.go's
// display layer has namespace/service available on every row) even
// with no downstream compute stage.
func TestPlanTrace_IncludeMetaVars(t *testing.T) {
	plan, err := Plan(`trace --id abc-123`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, col := range MetaVarColumns {
		if !strings.Contains(plan.ShardSQL, ", "+col) {
			t.Errorf("ShardSQL missing MetaVarColumns projection %q: %s", col, plan.ShardSQL)
		}
	}
}

// TestPlanTrace_GrammarErrors: malformed trace commands are rejected by the
// shared grammar (see the tracecmd package).
func TestPlanTrace_GrammarErrors(t *testing.T) {
	for _, pipeline := range []string{"trace id1 id2", "trace --id", "trace --id a --id b", "trace --name x", "trace --samples", "trace /log/acme/api"} {
		t.Run(pipeline, func(t *testing.T) {
			if _, err := Plan(pipeline, 1, "", time.Time{}, time.Time{}); err == nil {
				t.Errorf("Plan(%q): expected error, got nil", pipeline)
			}
		})
	}
}

func TestPlanTrace_SpansFlag(t *testing.T) {
	for _, pipeline := range []string{"trace --spans --id abc-123", "trace --id abc-123 --spans"} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, 1, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", pipeline, err)
			}
			if plan.Aggregate != AggregateSpanRows {
				t.Errorf("Aggregate = %q, want %q", plan.Aggregate, AggregateSpanRows)
			}
			for _, want := range []string{"trace_id = ?", "span_name IS NOT NULL", "(duration_ns > 0 OR level = 'ERROR')", "'parent_id', parent_span_id"} {
				if !strings.Contains(plan.ShardSQL, want) {
					t.Errorf("ShardSQL missing %q: %s", want, plan.ShardSQL)
				}
			}
			if strings.Contains(plan.ShardSQL, "CAST(event_id") {
				t.Errorf("ShardSQL still selects whole rows: %s", plan.ShardSQL)
			}
		})
	}
}

func TestPlanTraceSpans_Aggregate(t *testing.T) {
	plan, err := Plan(`trace --spans`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Aggregate != AggregateSpans {
		t.Errorf("Aggregate = %q, want %q", plan.Aggregate, AggregateSpans)
	}
	for _, want := range []string{"GROUP BY span_name, namespace, service, bucket", "span_name IS NOT NULL", "(duration_ns > 0 OR level = 'ERROR')", "count(*) FILTER (WHERE level = 'ERROR')"} {
		if !strings.Contains(plan.ShardSQL, want) {
			t.Errorf("ShardSQL missing %q: %s", want, plan.ShardSQL)
		}
	}
	for _, unwanted := range []string{"trace_id = ?", "span_name = ?", "namespace =", "service =", "kind =", "namespace IN", "service IN", "kind IN", "raw LIKE"} {
		if strings.Contains(plan.ShardSQL, unwanted) {
			t.Errorf("ShardSQL unexpectedly contains %q: %s", unwanted, plan.ShardSQL)
		}
	}
	if plan.SourceCmd != "trace_spans" {
		t.Errorf("SourceCmd: got %q, want trace_spans", plan.SourceCmd)
	}
}

func TestPlanTraceSpans_WithName(t *testing.T) {
	plan, err := Plan(`trace --spans --name shard.Query`, 1, "", time.Time{}, time.Time{})
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

// Grouping by service is the only behavior: the same span name in two
// services is two different things.
func TestPlanTraceSpans_GroupsByService(t *testing.T) {
	plan, err := Plan(`trace --spans`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "GROUP BY span_name, namespace, service, bucket") {
		t.Errorf("ShardSQL doesn't group by service: %s", plan.ShardSQL)
	}
	if !strings.Contains(plan.ShardSQL, "'namespace', namespace, 'service', service") {
		t.Errorf("ShardSQL doesn't return the service: %s", plan.ShardSQL)
	}
}

func TestPlanTraceSpans_Samples(t *testing.T) {
	plan, err := Plan(`trace --spans --name db.query --samples`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Aggregate != AggregateSpanSamples {
		t.Errorf("Aggregate = %q, want %q", plan.Aggregate, AggregateSpanSamples)
	}
	for _, want := range []string{"span_name = ?", "duration_ns > 0", "ORDER BY duration_ns DESC LIMIT 50"} {
		if !strings.Contains(plan.ShardSQL, want) {
			t.Errorf("ShardSQL missing %q: %s", want, plan.ShardSQL)
		}
	}
	if strings.Contains(plan.ShardSQL, "level = 'ERROR'") {
		t.Errorf("samples include error rows: %s", plan.ShardSQL)
	}
}

func TestPlanTraceSpans_StagesRunInCompute(t *testing.T) {
	plan, err := Plan(`trace --spans | jq 'select(.p99_ns > 1000000)'`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Contains(plan.ShardSQL, "p99_ns") {
		t.Errorf("jq select was pushed into the shard: %s", plan.ShardSQL)
	}
	if len(plan.ComputeStages) != 1 || !strings.HasPrefix(plan.ComputeStages[0], "jq") {
		t.Errorf("ComputeStages = %v, want the jq stage", plan.ComputeStages)
	}
}

func TestPlanTraceSpans_OptionErrors(t *testing.T) {
	for _, pipeline := range []string{"trace --spans --by service", "trace --spans --by host", "trace --spans --samples", "trace --spans --name a --samples --by service", "trace --spans --bogus"} {
		if _, err := Plan(pipeline, 1, "", time.Time{}, time.Time{}); err == nil {
			t.Errorf("Plan(%q): expected error, got nil", pipeline)
		}
	}
}

// TestPlanTraceSpans_ArityErrors: at most one span name argument.
func TestPlanTraceSpans_ArityErrors(t *testing.T) {
	if _, err := Plan(`trace --spans a b`, 1, "", time.Time{}, time.Time{}); err == nil {
		t.Errorf("Plan(%q): expected error, got nil", "trace --spans a b")
	}
}
