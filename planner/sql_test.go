package planner

import (
	"testing"
	"time"
)

// TestQueryBuilder_BareSelect covers the simplest output shape: no
// predicates, no order, no limit — just SELECT raw, ts FROM logs.
// Pins that the empty builder emits a valid query rather than a
// half-formed string.
func TestQueryBuilder_BareSelect(t *testing.T) {
	q := newSQLBuilder()
	got := q.build()
	want := "SELECT raw, ts FROM logs"
	if got != want {
		t.Fatalf("bare select:\n got: %s\nwant: %s", got, want)
	}
}

// TestQueryBuilder_AllClauses exercises every clause we render: an
// alternate rawExpr (projection), multiple ANDed predicates, ORDER BY,
// LIMIT, OFFSET. The order of clauses is the contract — render must
// emit them in standard SQL order regardless of the order setters
// were called in.
func TestQueryBuilder_AllClauses(t *testing.T) {
	q := newSQLBuilder()
	q.addPredicate("namespace = 'vimgym'")
	q.addPredicate("level = 'ERROR'")
	q.setOrderLimit("ts ASC", 100)
	q.setLimitOffset(100, 20) // overrides limit, adds offset
	q.setSelect("json_extract(raw, '$.msg')")

	got := q.build()
	want := "SELECT json_extract(raw, '$.msg') as raw, ts FROM logs WHERE namespace = 'vimgym' AND level = 'ERROR' ORDER BY ts ASC LIMIT 100 OFFSET 20"
	if got != want {
		t.Fatalf("full query:\n got: %s\nwant: %s", got, want)
	}
}

// TestQueryBuilder_CustomerAndBoundsOrder pins that addCustomer +
// addTimeBounds emit their predicates in the order they were called
// (the planner relies on this so customer = ? lands first in the
// WHERE clause — see customer_predicate_test.go for the security-
// boundary rationale). Args appear in render order.
func TestQueryBuilder_CustomerAndBoundsOrder(t *testing.T) {
	q := newSQLBuilder()
	q.addCustomer("kevlar")
	q.addTimeBounds(
		time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC),
	)
	q.addPredicate("namespace = ?", "vimgym")

	sql, args := q.ToSQL()
	wantSQL := "SELECT raw, ts FROM logs WHERE customer = ? AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP) AND namespace = ?"
	if sql != wantSQL {
		t.Fatalf("ordered predicates:\n got: %s\nwant: %s", sql, wantSQL)
	}
	wantArgs := []any{"kevlar", "2026-04-01 12:00:00", "2026-04-02 12:00:00", "vimgym"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args: got %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d]: got %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

// TestQueryBuilder_CustomerInjectionAttempt — a malicious customer name
// can't break out of the placeholder. Placeholders bind through DuckDB's
// driver as a single VARCHAR, so the `'; DROP TABLE...` payload lands
// as the literal value of the comparison, not as additional SQL.
func TestQueryBuilder_CustomerInjectionAttempt(t *testing.T) {
	q := newSQLBuilder()
	q.addCustomer("bobby'; DROP TABLE logs; --")

	sql, args := q.ToSQL()
	wantSQL := "SELECT raw, ts FROM logs WHERE customer = ?"
	if sql != wantSQL {
		t.Errorf("sql:\n got: %s\nwant: %s", sql, wantSQL)
	}
	if len(args) != 1 || args[0] != "bobby'; DROP TABLE logs; --" {
		t.Errorf("args: got %v, want exactly the malicious string", args)
	}
}

// TestQueryBuilder_Wrap covers the subquery flip used by tail: an
// outer SELECT pulls from the inner query as a subquery with an
// alias, applying its own ORDER BY. The inner query keeps all its
// own clauses (predicates, ORDER BY, LIMIT) verbatim. This is the
// shape Step 3 hands tail: inner does ts-DESC + LIMIT N, outer
// flips to ts-ASC for chronological display.
func TestQueryBuilder_Wrap(t *testing.T) {
	inner := newSQLBuilder()
	inner.addPredicate("namespace = 'vimgym'")
	inner.setOrderLimit("ts DESC", 50)

	outer := inner.Wrap("ts ASC")
	got := outer.build()
	want := "SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = 'vimgym' ORDER BY ts DESC LIMIT 50) AS t ORDER BY ts ASC"
	if got != want {
		t.Fatalf("wrap:\n got: %s\nwant: %s", got, want)
	}
}

// TestQueryBuilder_WrapPreservesInner asserts that Wrap returns a new
// outer Query without mutating the inner — the planner needs to be
// able to reason about the inner after wrapping (e.g. for the merge-key
// classification in shard_mode.go which inspects orderBy).
func TestQueryBuilder_WrapPreservesInner(t *testing.T) {
	inner := newSQLBuilder()
	inner.addPredicate("namespace = 'vimgym'")
	inner.setOrderLimit("ts DESC", 50)

	_ = inner.Wrap("ts ASC")

	if inner.orderBy != "ts DESC" {
		t.Errorf("inner orderBy mutated by Wrap: got %q", inner.orderBy)
	}
	if inner.limit != 50 {
		t.Errorf("inner limit mutated by Wrap: got %d", inner.limit)
	}
}

// TestQueryBuilder_ToSQLArgs locks the placeholder + args plumbing in
// place before Step 8 starts populating Predicate.Args. ToSQL collects
// args in render order: subquery WHEREs first (their placeholders
// appear first in the rendered string), then the outer WHERE. This
// matches positional binding semantics — once Predicate emitters
// switch to ? + Args, the binding order Just Works.
func TestQueryBuilder_ToSQLArgs(t *testing.T) {
	inner := newSQLBuilder()
	inner.predicates = append(inner.predicates, Predicate{
		SQL: "namespace = ?", Args: []ArgSource{{Kind: ArgLiteral, Value: "vimgym"}},
	})
	inner.predicates = append(inner.predicates, Predicate{
		SQL: "service = ?", Args: []ArgSource{{Kind: ArgLiteral, Value: "shard"}},
	})
	inner.setOrderLimit("ts DESC", 50)

	outer := inner.Wrap("ts ASC")
	outer.predicates = append(outer.predicates, Predicate{
		SQL: "ts >= ?", Args: []ArgSource{{Kind: ArgTimeStart, Value: "2026-04-01"}},
	})

	sql, args := outer.ToSQL()
	wantSQL := "SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? ORDER BY ts DESC LIMIT 50) AS t WHERE ts >= ? ORDER BY ts ASC"
	if sql != wantSQL {
		t.Errorf("ToSQL sql:\n got: %s\nwant: %s", sql, wantSQL)
	}
	wantArgs := []any{"vimgym", "shard", "2026-04-01"}
	if len(args) != len(wantArgs) {
		t.Fatalf("ToSQL args length: got %d %v, want %d %v", len(args), args, len(wantArgs), wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d]: got %v, want %v", i, args[i], wantArgs[i])
		}
	}
}

// TestQueryBuilder_BuildWithOverride covers the direct-to-compute
// path where the same WHERE/SELECT is rendered with a different
// ORDER BY + LIMIT than the builder's own settings. Both build()
// and buildWith() must leave the builder's stored fields untouched
// so a subsequent build() with no override behaves as expected.
func TestQueryBuilder_BuildWithOverride(t *testing.T) {
	q := newSQLBuilder()
	q.addPredicate("namespace = 'vimgym'")
	q.setOrderLimit("ts DESC", 50)

	direct := q.buildWith("raw", 0)
	wantDirect := "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' ORDER BY raw"
	if direct != wantDirect {
		t.Errorf("buildWith:\n got: %s\nwant: %s", direct, wantDirect)
	}

	again := q.build()
	wantOriginal := "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' ORDER BY ts DESC LIMIT 50"
	if again != wantOriginal {
		t.Errorf("build() after buildWith should be unchanged:\n got: %s\nwant: %s", again, wantOriginal)
	}
}
