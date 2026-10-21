package planner

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPlanTrace_ScopeNarrowsEveryForm(t *testing.T) {
	for _, pipeline := range []string{
		"trace acme/api",
		"trace --id abc acme/api",
		"trace --spans acme/api",
		"trace --spans --id abc acme/api",
		"trace --spans --name db.query acme/api",
	} {
		t.Run(pipeline, func(t *testing.T) {
			plan, err := Plan(pipeline, 1, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			for _, want := range []string{"namespace = ?", "service = ?"} {
				if !strings.Contains(plan.ShardSQL, want) {
					t.Errorf("ShardSQL missing %q: %s", want, plan.ShardSQL)
				}
			}
			if plan.SourceNamespace != "acme" || plan.SourceService != "api" {
				t.Errorf("routing = %q/%q, want acme/api", plan.SourceNamespace, plan.SourceService)
			}
		})
	}
}

func TestPlanTrace_NamespaceOnlyScope(t *testing.T) {
	plan, err := Plan("trace acme", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "namespace = ?") || strings.Contains(plan.ShardSQL, "service = ?") {
		t.Errorf("want a namespace predicate and no service predicate: %s", plan.ShardSQL)
	}
	if plan.SourceNamespace != "acme" || plan.SourceService != "" {
		t.Errorf("routing = %q/%q, want acme/", plan.SourceNamespace, plan.SourceService)
	}
}

// A bare `trace` lists recent traces in two steps: the newest IDs, then
// their stats (see traces.go for why one step is not exact).
func TestPlanTraceList_TwoSteps(t *testing.T) {
	plan, err := Plan("trace", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.SourceCmd != "trace_list" || plan.Aggregate != AggregateTraceIDs {
		t.Fatalf("SourceCmd/Aggregate = %q/%q", plan.SourceCmd, plan.Aggregate)
	}
	for _, want := range []string{
		"GROUP BY trace_id", "ORDER BY max(ts) DESC LIMIT 50", "trace_id IS NOT NULL", "trace_id <> ''",
	} {
		if !strings.Contains(plan.ShardSQL, want) {
			t.Errorf("step 1 SQL missing %q: %s", want, plan.ShardSQL)
		}
	}
	if plan.TraceStats == nil || plan.TraceStats.Aggregate != AggregateTraceList {
		t.Fatalf("TraceStats = %+v", plan.TraceStats)
	}
	for _, want := range []string{
		"GROUP BY trace_id", "trace_id IN (", "'lines', count(*)", "'services', list(DISTINCT namespace || '/' || service)",
		"count(*) FILTER (WHERE level = 'ERROR')", "count(*) FILTER (WHERE level <> '')", "'root'",
	} {
		if !strings.Contains(plan.TraceStats.SQL, want) {
			t.Errorf("step 2 SQL missing %q: %s", want, plan.TraceStats.SQL)
		}
	}
}

func TestPlanTraceList_StatsStepIsNotScoped(t *testing.T) {
	plan, err := Plan("trace acme/api", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "namespace = ?") {
		t.Errorf("step 1 should pick the traces that touched acme/api: %s", plan.ShardSQL)
	}
	if strings.Contains(plan.TraceStats.SQL, "namespace = ?") || strings.Contains(plan.TraceStats.SQL, "service = ?") {
		t.Errorf("step 2 summarizes the whole trace, so it must not be scoped: %s", plan.TraceStats.SQL)
	}
}

func TestTraceStatsQuery_WithIDsPads(t *testing.T) {
	plan, err := Plan("trace", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	args := plan.TraceStats.WithIDs([]string{"a", "b"})
	if got, want := strings.Count(plan.TraceStats.SQL, "?"), len(args); got != want {
		t.Fatalf("SQL has %d placeholders, WithIDs gave %d args", got, want)
	}
	n := len(plan.TraceStats.Args)
	if args[n] != "a" || args[n+1] != "b" || args[n+2] != "" || args[len(args)-1] != "" {
		t.Errorf("ids not placed and padded: %v", args[n:])
	}
	if len(args)-n != TraceListLimit {
		t.Errorf("%d id slots, want %d", len(args)-n, TraceListLimit)
	}
}

func TestPlanTraceList_StagesRunInCompute(t *testing.T) {
	plan, err := Plan(`trace | jq 'select(.lines > 100)'`, 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.ComputeStages) != 1 || !strings.HasPrefix(plan.ComputeStages[0], "jq") {
		t.Errorf("ComputeStages = %v, want the jq stage", plan.ComputeStages)
	}
}

func TestLower_RejectsTheTraceList(t *testing.T) {
	if _, err := Lower("trace", 1, "", nil); err == nil || !strings.Contains(err.Error(), "two steps") {
		t.Errorf("Lower(trace) = %v, want a two-step refusal", err)
	}
}

// Shards hold every customer's rows, so the customer predicate is the only
// thing keeping one org out of another's traces. Both steps must carry it,
// including the unscoped stats step.
func TestPlanTraceList_BothStepsAreOrgScoped(t *testing.T) {
	plan, err := Plan("trace acme/api", 42, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.SourceCustomerID != 42 {
		t.Errorf("SourceCustomerID = %d, want 42", plan.SourceCustomerID)
	}
	if !strings.Contains(plan.ShardSQL, "customer_id = CAST(? AS BIGINT)") {
		t.Errorf("step 1 is not org scoped: %s", plan.ShardSQL)
	}
	if !strings.Contains(plan.TraceStats.SQL, "customer_id = CAST(? AS BIGINT)") {
		t.Errorf("step 2 is not org scoped: %s", plan.TraceStats.SQL)
	}
	if len(plan.TraceStats.Args) == 0 || fmt.Sprint(plan.TraceStats.Args[0]) != "42" {
		t.Errorf("step 2 args don't start with customer 42: %v", plan.TraceStats.Args)
	}
}
