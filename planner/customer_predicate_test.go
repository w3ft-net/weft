package planner

import (
	"strings"
	"testing"
	"time"
)

// TestPlanCustomer_HappyPath: a normal customer name lands as the first
// predicate in the WHERE clause, bound via a placeholder.
func TestPlanCustomer_HappyPath(t *testing.T) {
	plan, err := Plan(`cat foo.log`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantSQL := `SELECT raw, ts FROM logs WHERE customer = ? AND namespace = ?`
	if plan.ShardSQL != wantSQL {
		t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, wantSQL)
	}
	wantArgs := []any{"kevlar", "foo"}
	if len(plan.Args) != len(wantArgs) || plan.Args[0] != wantArgs[0] || plan.Args[1] != wantArgs[1] {
		t.Errorf("Args: got %v, want %v", plan.Args, wantArgs)
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

// TestPlanCustomer_Injection: a hostile customer name carrying SQL
// metacharacters lands as a bound positional value, not as inline SQL.
// Pre-parameterization this property was held by a manual quote-double
// in addCustomer; now the placeholder makes it structural.
func TestPlanCustomer_Injection(t *testing.T) {
	plan, err := Plan(`cat foo.log`, `acme'; DROP TABLE logs; --`, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "customer = ?") {
		t.Errorf("customer predicate missing placeholder: %s", plan.ShardSQL)
	}
	if len(plan.Args) == 0 || plan.Args[0] != `acme'; DROP TABLE logs; --` {
		t.Errorf("customer arg: got %v, want exactly the malicious string", plan.Args)
	}
}

// TestPlanJQMetaVar_Escaping is the regression guard for a pre-existing
// bug in jqComparisonToSQL's meta-variable branch: unlike the adjacent
// regular-JSON-field branch three lines below it, the meta-var branch
// interpolated its literal directly with no escapeSQLString call. A
// literal containing a single quote (e.g. a customer-supplied trace ID
// copy-pasted from an upstream system) would land in the SQL unescaped.
// This test would fail before the jq.go:118 fix and pass after.
func TestPlanJQMetaVar_Escaping(t *testing.T) {
	plan, err := Plan(`cat vimgym.log | jq 'select(._trace_id == "trace-o'"'"'brien-123")'`, "kevlar", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "trace_id = 'trace-o''brien-123'") {
		t.Errorf("expected escaped (doubled) quote in meta-var SQL, got: %s", plan.ShardSQL)
	}
}

// TestPlanTimeBounds_BothSet emits ts >= and ts <= predicates with
// CAST(? AS TIMESTAMP) placeholders when both bounds are non-zero. The
// timestamp value is shipped as a string arg over the wire so we don't
// need typed proto args. The clamp at the call site ensures the lower
// bound has already been advanced to the tier's retention cap.
func TestPlanTimeBounds_BothSet(t *testing.T) {
	start := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	plan, err := Plan(`cat foo.log`, "kevlar", start, end)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !strings.Contains(plan.ShardSQL, "ts >= CAST(? AS TIMESTAMP)") {
		t.Errorf("missing lower bound predicate: %s", plan.ShardSQL)
	}
	if !strings.Contains(plan.ShardSQL, "ts <= CAST(? AS TIMESTAMP)") {
		t.Errorf("missing upper bound predicate: %s", plan.ShardSQL)
	}
	// Args order: customer, start, end, namespace.
	wantArgs := []any{"kevlar", "2026-04-01 12:00:00", "2026-04-02 12:00:00", "foo"}
	if len(plan.Args) != len(wantArgs) {
		t.Fatalf("Args length: got %v, want %v", plan.Args, wantArgs)
	}
	for i, w := range wantArgs {
		if plan.Args[i] != w {
			t.Errorf("Args[%d]: got %v, want %v", i, plan.Args[i], w)
		}
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
	if !strings.Contains(plan.ShardSQL, "ts >= CAST(? AS TIMESTAMP)") {
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
	if !strings.HasPrefix(clause, "customer = ?") {
		t.Errorf("customer predicate not at head of inner WHERE:\n  rendered: %s\n  clause start: %q", sql, clause[:min(60, len(clause))])
	}
	if len(plan.Args) == 0 || plan.Args[0] != "tenant-a" {
		t.Errorf("customer arg: got %v, want first arg to be 'tenant-a'", plan.Args)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
