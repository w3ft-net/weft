package planner

import (
	"strings"
	"testing"
	"time"
)

// TestPlanCustomer_HappyPath: a normal customer name lands as the first
// predicate in the WHERE clause.
func TestPlanCustomer_HappyPath(t *testing.T) {
	plan, err := Plan(`cat foo.log`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := `SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND (app = 'foo' OR app LIKE 'foo/%')`
	if plan.ShardSQL != want {
		t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, want)
	}
}

// TestPlanCustomer_EmptySkipsPredicate: empty customer (test fixture
// shape) emits no customer predicate. Real production code never
// passes "" — the coordinator threads the JWT-resolved name through.
func TestPlanCustomer_EmptySkipsPredicate(t *testing.T) {
	plan, err := Plan(`cat foo.log`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Contains(plan.ShardSQL, "customer") {
		t.Errorf("empty customer leaked predicate: %s", plan.ShardSQL)
	}
}

// TestPlanCustomer_QuoteEscape: an apostrophe in the customer name is
// doubled per SQL string-literal escape rules. Without this guard, a
// hostile customer name like `acme' OR 1=1 --` would close the literal
// early. The escape moved from the (now-deleted) InjectCustomer helper
// into addCustomer; this test pins it where the new code lives.
func TestPlanCustomer_QuoteEscape(t *testing.T) {
	plan, err := Plan(`cat foo.log`, `acme's-team`, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "customer = 'acme''s-team'") {
		t.Errorf("apostrophe not doubled in customer literal: %s", plan.ShardSQL)
	}
}

// TestPlanTimeBounds_BothSet bakes ts >= and ts <= predicates into the
// SQL when both bounds are non-zero. The clamp at the call site ensures
// the lower bound has already been advanced to the tier's retention cap.
func TestPlanTimeBounds_BothSet(t *testing.T) {
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	plan, err := Plan(`cat foo.log`, "kevlar", start, end)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "ts >= TIMESTAMP '2026-04-01 12:00:00'") {
		t.Errorf("missing lower bound predicate: %s", plan.ShardSQL)
	}
	if !strings.Contains(plan.ShardSQL, "ts <= TIMESTAMP '2026-04-02 12:00:00'") {
		t.Errorf("missing upper bound predicate: %s", plan.ShardSQL)
	}
}

// TestPlanTimeBounds_LowerOnly emits only the ts >= predicate when end
// is zero-value. cron and follow-mode callers default end to far-future
// or zero respectively; either should produce a sensible plan.
func TestPlanTimeBounds_LowerOnly(t *testing.T) {
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	plan, err := Plan(`cat foo.log`, "", start, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "ts >= TIMESTAMP '2026-04-01 12:00:00'") {
		t.Errorf("missing lower bound: %s", plan.ShardSQL)
	}
	if strings.Contains(plan.ShardSQL, "ts <= ") {
		t.Errorf("upper bound leaked when end is zero: %s", plan.ShardSQL)
	}
}

// TestPlanTimeBounds_BothZero: zero/zero leaves the SQL with no time
// predicates. Every test in the planner package today passes
// time.Time{}, time.Time{} — they all rely on this property to keep
// their SQL goldens stable.
func TestPlanTimeBounds_BothZero(t *testing.T) {
	plan, err := Plan(`cat foo.log`, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Contains(plan.ShardSQL, "ts >= ") || strings.Contains(plan.ShardSQL, "ts <= ") {
		t.Errorf("ts predicate leaked with zero bounds: %s", plan.ShardSQL)
	}
}

// TestPlanCustomer_InsideLimitBoundedSubquery is the regression guard
// for the property the old InjectCustomer enforced by anchoring on the
// inner FROM logs. A LIMIT-bound subquery shape (terminal sort with
// raw projection forces nesting in DirectShardSQL) must keep the
// customer predicate INSIDE the inner SELECT — outside, the LIMIT
// would let cross-tenant rows leak through before the customer filter
// runs.
//
// We rely on parsing the rendered SQL: locate "FROM logs", then assert
// the next WHERE keyword that follows on the same nesting level
// contains "customer = '...'".
func TestPlanCustomer_InsideLimitBoundedSubquery(t *testing.T) {
	// `cat | sort | head` emits a subquery via direct-to-compute
	// classification (sort terminal + non-trivial projection).
	plan, err := Plan(`cat foo.log | sort | head -5`, "tenant-a", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	sql := plan.DirectShardSQL
	idx := strings.Index(sql, "FROM logs")
	if idx == -1 {
		t.Fatalf("expected FROM logs in DirectShardSQL: %s", sql)
	}
	rest := sql[idx:]
	whereIdx := strings.Index(rest, " WHERE ")
	if whereIdx == -1 {
		t.Fatalf("expected WHERE clause: %s", sql)
	}
	clause := rest[whereIdx+len(" WHERE "):]
	if !strings.HasPrefix(clause, "customer = 'tenant-a'") {
		t.Errorf("customer predicate not at head of inner WHERE:\n  rendered: %s\n  clause start: %q", sql, clause[:min(60, len(clause))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
