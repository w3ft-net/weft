package planner

import (
	"fmt"
	"strconv"
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
type Query struct {
	rawExpr    string
	from       From
	predicates []Predicate
	orderBy    string
	limit      int
	offset     int
	// includeMetaVars appends MetaVarColumns to the SELECT list — set by
	// Plan when a remaining jq compute stage might read a bare
	// "._"-prefixed meta-variable for output. See planner.go's
	// includeMetaVars detection and docs/known-issues.md's (fixed) jq
	// meta-variable entry.
	includeMetaVars bool
	// includePayload appends the row's payload as a PayloadColumn
	// column, for a jq row stage or a stage reading the payload.
	includePayload bool
	// aggregate makes this the shard half of a QueryPlan.Aggregate: it
	// groups the lines of its subquery's raw (see Aggregate).
	aggregate string
}

// PayloadColumn names the extra column carrying a row's payload JSON
// text when a row stage or the first stage reads it. Shards return
// extra columns in ResultRow.MetaVars, keyed by name.
const PayloadColumn = "payload"

// EventIDColumn and LineNoColumn name the columns every row query returns
// after (raw, ts): the stored row's event grouping, empty/NULL on a
// one-line event. ScanResultRows moves them into ResultRow's typed
// fields rather than MetaVars.
const (
	EventIDColumn = "event_id"
	LineNoColumn  = "line_no"
)

// orderClause expands a ts order into the full (ts, event_id, line_no)
// ordering, one-line events (NULL event_id) first. DESC is the exact
// reverse, so tail's inner DESC LIMIT and outer ASC re-sort agree.
// Other orders (raw, raw DESC) pass through.
func orderClause(order string) string {
	switch order {
	case "ts", "ts ASC":
		return "ts, event_id NULLS FIRST, line_no NULLS FIRST"
	case "ts DESC":
		return "ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST"
	}
	return order
}

// CountColumn names the extra column carrying an aggregate's count: the
// lines a shard saw of each distinct raw (or of all of them, for
// AggregateLines).
const CountColumn = "count"

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

// addCustomer attaches the row-level security predicate. A zero id is a
// no-op (no production caller passes zero; tests do). Tagged ArgCustomer so
// a save-time-lowered form resolves this from the watch's stored OrgID at
// fire time, not from whatever customer was active during Plan().
//
// Args travel as strings (see ArgSource), so the placeholder is cast
// explicitly, the same way the timestamp bounds are: it must not depend on
// how DuckDB would coerce a VARCHAR parameter against a BIGINT column.
func (q *Query) addCustomer(customerID int64) {
	if customerID == 0 {
		return
	}
	q.predicates = append(q.predicates, Predicate{
		SQL:  "customer_id = CAST(? AS BIGINT)",
		Args: []ArgSource{{Kind: ArgCustomer, Value: strconv.FormatInt(customerID, 10)}},
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
}

func (q *Query) setLimitOffset(limit, offset int) {
	q.limit = limit
	q.offset = offset
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
		includePayload:  q.includePayload,
	}
}

// Lines returns q's rows split into the lines compute would feed a stage
// (each row's raw, or q's projection, split on newlines), keeping the
// lines where every pred holds. Each line keeps its row's ts and columns.
// q must have no limit.
func (q *Query) Lines(preds []Predicate) *Query {
	return &Query{
		rawExpr:         "raw",
		from:            From{Subquery: q.lineSplit(), Alias: "t"},
		predicates:      preds,
		includeMetaVars: q.includeMetaVars,
		includePayload:  q.includePayload,
	}
}

// Aggregate returns the shard half of an aggregate over q's lines, as
// Lines splits and filters them. It selects each line with its
// CountColumn (AggregateLines: one row, an empty line with the total),
// in the order compute merges on. q must have no limit; its
// meta-variable and payload columns are dropped.
func (q *Query) Aggregate(fn, orderBy string, preds []Predicate) *Query {
	inner := q.lineSplit()
	inner.includeMetaVars, inner.includePayload = false, false
	return &Query{
		from:       From{Subquery: inner, Alias: "t"},
		predicates: preds,
		orderBy:    orderBy,
		aggregate:  fn,
	}
}

// lineSplit is q selecting one row per line of its raw.
func (q *Query) lineSplit() *Query {
	inner := *q
	inner.rawExpr = "unnest(string_split(" + q.rawExpr + ", chr(10)))"
	return &inner
}

// WithOrderLimit returns a shallow clone of q with the orderBy and
// limit fields overridden. The predicate slice is shared (safe — neither
// caller mutates predicates after this point). Used by the direct-to-
// compute path so the same WHERE clause can be rendered with the mode-
// classified ORDER BY/LIMIT from sort/limit fusion rather than the
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
	if q.aggregate != "" {
		key := "raw"
		if q.aggregate == AggregateLines {
			key = "'' AS raw"
		}
		fmt.Fprintf(b, "%s, min(ts) AS ts, count(*) AS %s FROM (", key, CountColumn)
		q.from.Subquery.renderInto(b, onArg)
		b.WriteString(") AS " + q.from.Alias)
		q.renderWhere(b, onArg)
		if q.aggregate == AggregateLines {
			// No row without lines: ts can't be NULL.
			b.WriteString(" HAVING count(*) > 0")
		} else {
			b.WriteString(" GROUP BY raw")
		}
		if q.orderBy != "" {
			fmt.Fprintf(b, " ORDER BY %s", q.orderBy)
		}
		return
	}
	if q.rawExpr == "" || q.rawExpr == "raw" {
		b.WriteString("raw, ts")
	} else {
		fmt.Fprintf(b, "%s as raw, ts", q.rawExpr)
	}
	// Every row carries its event grouping so ts ties order the same way
	// in the shard, in a wrapping query, and in compute's merge.
	if q.from.Subquery != nil {
		b.WriteString(", " + EventIDColumn + ", " + LineNoColumn)
	} else {
		b.WriteString(", CAST(event_id AS VARCHAR) AS " + EventIDColumn + ", " + LineNoColumn)
	}
	if q.includeMetaVars {
		for _, col := range MetaVarColumns {
			b.WriteString(", ")
			b.WriteString(col)
		}
	}
	if q.includePayload {
		if q.from.Subquery != nil {
			b.WriteString(", " + PayloadColumn)
		} else {
			b.WriteString(", CAST(payload AS VARCHAR) AS " + PayloadColumn)
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
	q.renderWhere(b, onArg)
	if q.orderBy != "" {
		fmt.Fprintf(b, " ORDER BY %s", orderClause(q.orderBy))
	}
	if q.limit > 0 {
		fmt.Fprintf(b, " LIMIT %d", q.limit)
	}
	if q.offset > 0 {
		fmt.Fprintf(b, " OFFSET %d", q.offset)
	}
}

func (q *Query) renderWhere(b *strings.Builder, onArg func(ArgSource)) {
	if len(q.predicates) == 0 {
		return
	}
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
