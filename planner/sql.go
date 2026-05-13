package planner

import (
	"fmt"
	"strings"
	"time"
)

// Query is the AST node for the SQL the planner emits at each shard.
// One node holds one SELECT; subqueries are *Query values in From,
// nested at the parser level rather than concatenated as strings. The
// shape covers everything the planner generates today (logs source,
// ANDed predicates, ORDER BY, LIMIT, OFFSET) plus the tail subquery
// flip (Wrap) — there is no surface beyond that on purpose. Stages in
// awk.go / grep.go / sed.go / jq.go / project.go still compose the
// predicate SQL fragments themselves (regexp_matches, list_extract,
// etc.); this type just glues them together with deterministic
// clause order.
//
// Predicate carries an Args slot so we can plumb positional placeholder
// parameters through in a later step without touching predicate
// emitters again. Today every helper passes Args == nil (or omits it)
// and ToSQL's output is byte-identical to the old string-concat
// sqlBuilder — the 252 pinned wantSQL assertions across the planner
// tests are the parity gate.
//
// limitFrozen mirrors the old sqlBuilder flag: once any LIMIT pushdown
// lands (source head/tail, stage head/tail, or awk NR-window), the
// planner stops pushing further predicates shard-side and routes them
// to compute. See docs/plans/cross-shard-post-limit-filter.md.
type Query struct {
	rawExpr     string
	from        From
	predicates  []Predicate
	orderBy     string
	limit       int
	offset      int
	limitFrozen bool
}

// From identifies the SELECT's source — either a base table ("logs")
// or a nested *Query rendered as a subquery with an alias. Exactly
// one of Table / Subquery is set.
type From struct {
	Table    string
	Subquery *Query
	Alias    string
}

// Predicate is one WHERE fragment with its positional args. Args is
// nil today; Step 7 plumbs it through ToSQL. The SQL field holds the
// raw fragment (e.g. "namespace = 'vimgym'" today; "namespace = ?"
// after parameterization).
type Predicate struct {
	SQL  string
	Args []any
}

// newSQLBuilder returns a fresh Query rooted at the logs table with
// the default raw-column projection. The name matches the old
// sqlBuilder constructor so planner.go's call site is a drop-in.
func newSQLBuilder() *Query {
	return &Query{
		rawExpr: "raw",
		from:    From{Table: "logs"},
	}
}

func (q *Query) setSelect(rawExpr string) { q.rawExpr = rawExpr }

// addPredicate appends a WHERE fragment with optional positional args.
// Variadic args is the migration ergonomic: callers that haven't yet
// switched to placeholders pass no args (the existing string form);
// callers that have pass `?` placeholders + values. The args are
// rendered out by ToSQL in their original order — see
// TestQueryBuilder_ToSQLArgs for the recursion contract.
func (q *Query) addPredicate(pred string, args ...any) {
	q.predicates = append(q.predicates, Predicate{SQL: pred, Args: args})
}

// addCustomer attaches the row-level security predicate. Empty
// customer is a no-op (no production caller passes empty; tests do).
func (q *Query) addCustomer(customer string) {
	if customer == "" {
		return
	}
	q.addPredicate("customer = ?", customer)
}

// addTimeBounds attaches ts >= / ts <= predicates for the user's query
// window. Either bound may be zero-value, in which case it's skipped.
// The cast wrapping (CAST(? AS TIMESTAMP)) lets DuckDB accept the
// timestamp as a string arg over the wire — string is the only proto
// arg type we ship (see ExecuteQueryRequest.args). The DuckDB-friendly
// ISO format uses .999999 microseconds to match the shard's ts column
// resolution.
func (q *Query) addTimeBounds(start, end time.Time) {
	if !start.IsZero() {
		q.addPredicate("ts >= CAST(? AS TIMESTAMP)", start.UTC().Format("2006-01-02 15:04:05.999999"))
	}
	if !end.IsZero() {
		q.addPredicate("ts <= CAST(? AS TIMESTAMP)", end.UTC().Format("2006-01-02 15:04:05.999999"))
	}
}

func (q *Query) setOrderLimit(order string, limit int) {
	q.orderBy = order
	q.limit = limit
	q.limitFrozen = true
}

func (q *Query) setLimitOffset(limit, offset int) {
	q.limit = limit
	q.offset = offset
	q.limitFrozen = true
}

// Wrap returns a new outer Query that selects from q as a subquery
// with its own ORDER BY. The inner Query is referenced by pointer
// without copy — callers must not mutate it after wrapping. Used by
// tail's pushdown: the inner runs ts-DESC + LIMIT N to grab the last
// N rows per shard cheaply; the outer flips to ts-ASC so the user
// sees them in chronological order. The alias "t" is conventional
// and arbitrary.
func (q *Query) Wrap(orderBy string) *Query {
	return &Query{
		rawExpr: "raw",
		from:    From{Subquery: q, Alias: "t"},
		orderBy: orderBy,
	}
}

// WithOrderLimit returns a shallow clone of q with the orderBy and
// limit fields overridden. The predicate slice is shared (safe — neither
// caller mutates predicates after this point). Used by the direct-to-
// compute path so the same WHERE clause can be rendered with the mode-
// classified ORDER BY/LIMIT from classifyShardMode rather than the
// head/tail-derived values stored on the original builder.
func (q *Query) WithOrderLimit(order string, limit int) *Query {
	clone := *q
	clone.orderBy = order
	clone.limit = limit
	return &clone
}

// build renders the SQL string. Equivalent to the old sqlBuilder.build:
// SELECT {rawExpr} as raw, ts FROM {from} [WHERE ...] [ORDER BY ...]
// [LIMIT N] [OFFSET N]. Args are discarded — callers that need them
// use ToSQL() instead. Step 8 starts populating Predicate.Args; Step 9
// switches the shard exec path to ToSQL.
func (q *Query) build() string {
	sql, _ := q.ToSQL()
	return sql
}

// buildWith renders SQL with the given orderBy + limit overriding the
// builder's own. Thin wrapper around WithOrderLimit + build for callers
// (e.g. tests) that want the string in one shot.
func (q *Query) buildWith(orderBy string, limit int) string {
	return q.WithOrderLimit(orderBy, limit).build()
}

// ToSQL walks the AST and emits (sql, args). Predicate fragments come
// through as-is; subqueries recurse with parentheses + alias. Each
// Predicate's Args are appended to the returned slice in render order
// — outer SELECT's WHERE first, then any recursive Predicate.Args from
// a subquery's WHERE in their original position relative to the
// placeholders, and so on. Today every Predicate carries Args == nil
// so the slice always comes back empty; Step 8 lights this up.
func (q *Query) ToSQL() (string, []any) {
	var b strings.Builder
	var args []any
	q.renderInto(&b, &args)
	return b.String(), args
}

func (q *Query) renderInto(b *strings.Builder, args *[]any) {
	b.WriteString("SELECT ")
	if q.rawExpr == "" || q.rawExpr == "raw" {
		b.WriteString("raw, ts")
	} else {
		fmt.Fprintf(b, "%s as raw, ts", q.rawExpr)
	}
	b.WriteString(" FROM ")
	if q.from.Subquery != nil {
		b.WriteByte('(')
		// Subquery's predicates emit their placeholders before the outer
		// query's. Args collection order = render order, so the subquery
		// recurses into the same args slice.
		q.from.Subquery.renderInto(b, args)
		b.WriteString(") AS ")
		b.WriteString(q.from.Alias)
	} else {
		b.WriteString(q.from.Table)
	}
	if len(q.predicates) > 0 {
		b.WriteString(" WHERE ")
		for i, p := range q.predicates {
			if i > 0 {
				b.WriteString(" AND ")
			}
			b.WriteString(p.SQL)
			if len(p.Args) > 0 {
				*args = append(*args, p.Args...)
			}
		}
	}
	if q.orderBy != "" {
		fmt.Fprintf(b, " ORDER BY %s", q.orderBy)
	}
	if q.limit > 0 {
		fmt.Fprintf(b, " LIMIT %d", q.limit)
	}
	if q.offset > 0 {
		fmt.Fprintf(b, " OFFSET %d", q.offset)
	}
}
