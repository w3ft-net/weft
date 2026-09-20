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
	// includeMetaVars appends MetaVarColumns to the SELECT list — set by
	// Plan when a remaining jq compute stage might read a bare
	// "._"-prefixed meta-variable for output. See planner.go's
	// includeMetaVars detection and docs/known-issues.md's (fixed) jq
	// meta-variable entry.
	includeMetaVars bool
}

// From identifies the SELECT's source — either a base table ("logs")
// or a nested *Query rendered as a subquery with an alias. Exactly
// one of Table / Subquery is set.
type From struct {
	Table    string
	Subquery *Query
	Alias    string
}

// Predicate is one WHERE fragment with its positional args. The SQL
// field holds the raw fragment ("namespace = ?"); each Args entry
// carries both the resolved value (what ToSQL needs to ship to shards
// today) and the source kind (what ToLowered needs to know about for
// save-time lowering).
type Predicate struct {
	SQL  string
	Args []ArgSource
}

// ArgKind tags how a positional `?` placeholder gets resolved. The
// distinction matters at save-time lowering: literals get baked into
// the persisted form; the runtime-context kinds (Customer, TimeStart,
// TimeEnd) get resolved fresh at each fire from the watch's customer
// and the fire's time window.
type ArgKind int

const (
	// ArgLiteral is a value baked into the pipeline (namespace name,
	// service name, env, etc.). The pipeline can't reference it at
	// fire time — it IS the pipeline.
	ArgLiteral ArgKind = iota
	// ArgCustomer is the row-level-security boundary. Resolved from
	// the watch's stored OrgID at fire time (or the active
	// session's customer for interactive queries).
	ArgCustomer
	// ArgTimeStart / ArgTimeEnd are the time window. Resolved from
	// the fire's TimeRange.
	ArgTimeStart
	ArgTimeEnd
)

// ArgSource describes one positional argument. Value carries the
// resolved value as of the Plan() that produced this source —
// for ArgLiteral that's the only place the value comes from;
// for ArgCustomer/TimeStart/TimeEnd that's the current call's
// resolved value but a save-time-lowering consumer ignores Value
// and re-resolves from fire-time context.
type ArgSource struct {
	Kind  ArgKind
	Value string
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

// addPredicate appends a WHERE fragment whose `?` placeholders are
// literal values from the pipeline (namespace name, service name,
// etc.). Use addCustomer / addTimeBounds for the runtime-resolved
// sources — those tag Args differently so save-time lowering can
// distinguish bake-at-save from resolve-at-fire.
func (q *Query) addPredicate(pred string, literals ...string) {
	args := make([]ArgSource, len(literals))
	for i, v := range literals {
		args[i] = ArgSource{Kind: ArgLiteral, Value: v}
	}
	q.predicates = append(q.predicates, Predicate{SQL: pred, Args: args})
}

// addCustomer attaches the row-level security predicate. Empty
// customer is a no-op (no production caller passes empty; tests do).
// Tagged ArgCustomer so a save-time-lowered form resolves this from
// the watch's stored OrgID at fire time, not from whatever
// customer was active during Plan().
func (q *Query) addCustomer(customer string) {
	if customer == "" {
		return
	}
	q.predicates = append(q.predicates, Predicate{
		SQL:  "customer = ?",
		Args: []ArgSource{{Kind: ArgCustomer, Value: customer}},
	})
}

// addTimeBounds attaches ts >= / ts <= predicates for the user's query
// window. Either bound may be zero-value, in which case it's skipped.
// Tagged ArgTimeStart / ArgTimeEnd so save-time lowering re-resolves
// from each fire's TimeRange rather than baking in the Plan-call time
// bounds.
func (q *Query) addTimeBounds(start, end time.Time) {
	if !start.IsZero() {
		q.predicates = append(q.predicates, Predicate{
			SQL:  "ts >= CAST(? AS TIMESTAMP)",
			Args: []ArgSource{{Kind: ArgTimeStart, Value: formatTS(start)}},
		})
	}
	if !end.IsZero() {
		q.predicates = append(q.predicates, Predicate{
			SQL:  "ts <= CAST(? AS TIMESTAMP)",
			Args: []ArgSource{{Kind: ArgTimeEnd, Value: formatTS(end)}},
		})
	}
}

// formatTS renders a time.Time as DuckDB-friendly ISO with microsecond
// precision (matching the shard's ts column resolution). The format
// lives here so save-time lowering and live dispatch produce identical
// argument strings for the same time.Time.
func formatTS(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999")
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
		rawExpr:         "raw",
		from:            From{Subquery: q, Alias: "t"},
		orderBy:         orderBy,
		includeMetaVars: q.includeMetaVars,
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

// ToSQL walks the AST and emits (sql, args) using each ArgSource's
// resolved-at-Plan-time Value. This is what the coordinator ships to
// shards. Subqueries recurse into the same args slice, so render
// order matches placeholder order.
func (q *Query) ToSQL() (string, []any) {
	var b strings.Builder
	var args []any
	q.renderInto(&b, func(src ArgSource) {
		args = append(args, src.Value)
	})
	return b.String(), args
}

// ToLowered walks the AST and emits (sql, sources). Sources carry the
// kind (Customer / TimeStart / TimeEnd / Literal) rather than resolved
// values — a save-time-lowered consumer re-resolves runtime kinds at
// each fire and uses Value as-is for ArgLiteral.
func (q *Query) ToLowered() (string, []ArgSource) {
	var b strings.Builder
	var sources []ArgSource
	q.renderInto(&b, func(src ArgSource) {
		sources = append(sources, src)
	})
	return b.String(), sources
}

// ToSQLWithSources renders once and returns both views. Plan() uses
// this so it can populate QueryPlan.Args (for live dispatch) and
// QueryPlan.ArgSources (for save-time lowering) without walking the
// AST twice.
func (q *Query) ToSQLWithSources() (sql string, args []any, sources []ArgSource) {
	var b strings.Builder
	q.renderInto(&b, func(src ArgSource) {
		args = append(args, src.Value)
		sources = append(sources, src)
	})
	return b.String(), args, sources
}

// renderInto is the shared rendering loop for ToSQL and ToLowered. The
// onArg callback is invoked once per arg in render order; the two
// public methods supply different callbacks to collect the form of
// arg they need.
func (q *Query) renderInto(b *strings.Builder, onArg func(ArgSource)) {
	b.WriteString("SELECT ")
	if q.rawExpr == "" || q.rawExpr == "raw" {
		b.WriteString("raw, ts")
	} else {
		fmt.Fprintf(b, "%s as raw, ts", q.rawExpr)
	}
	if q.includeMetaVars {
		for _, col := range MetaVarColumns {
			b.WriteString(", ")
			b.WriteString(col)
		}
	}
	b.WriteString(" FROM ")
	if q.from.Subquery != nil {
		b.WriteByte('(')
		q.from.Subquery.renderInto(b, onArg)
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
			for _, a := range p.Args {
				onArg(a)
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
