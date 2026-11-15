package planner

import (
	"fmt"
	"strings"
	"time"

	coords "github.com/w3ft-net/weft/shape"
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
	// metaCols are the meta-variable columns appended to the SELECT list: the
	// ones the stages that run in compute read (MetaRead.Columns), set by Plan.
	metaCols []string
	// includePayload appends the row's payload as a PayloadColumn
	// column, for a jq row stage or a stage reading the payload.
	includePayload bool
	// fieldSpec makes this the field form of the query: the SELECT also
	// returns the values of the fields it names (see fieldForm).
	fieldSpec *fieldReads
	// group makes this the shard half of a QueryPlan.Group: it groups the
	// lines of its subquery's raw (see Group).
	group GroupRender
	// agg, on a group's subquery and on the group itself, is the awk
	// aggregate they compute the value of (see Group).
	agg *keyedAgg
	// spanKind makes this a span query (see spans.go): the shard half of
	// one of the AggregateSpan* aggregates, rendered straight from the
	// table.
	spanKind string
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

// The field form's columns, after the ones every row query returns. They
// carry the values of the fields a stage reads, in the spec's order, and the
// row's field count; ScanResultRows moves them into ResultRow.Fields and
// FieldCount, and compute.RebuildRecord makes the line from them.
const (
	FieldColumnPrefix = "field_"
	FieldLastColumn   = "field_last"
	FieldCountColumn  = "nf"
)

// CountColumn names the extra column carrying an aggregate's count: the
// lines a shard saw of each distinct raw (or of all of them, for
// AggregateLines).
const CountColumn = "count"

// BytesColumn names the extra column carrying a RenderBytes aggregate's
// byte total: each line's bytes and the newline wc counts after it.
const BytesColumn = "bytes"

// The columns an awk aggregate's group query adds (see keyedAgg). The inner
// query selects the value of the field beside each line (ValColumn: a BIGINT for
// a sum, a DOUBLE for a max or min, and for those TxtColumn, the field's text);
// the group returns, per key:
//
//   - a sum: SumColumn, the exact integer total as decimal text (DuckDB adds
//     BIGINTs in 128 bits, so it cannot round or overflow);
//   - a max or min: BestColumn (the greatest or least number), BestTxtColumn
//     (the text of the first row, in merge order, holding it: awk assigns the
//     field, so it prints as it was written) and BestNsColumn (that row's ts, for
//     ties between shards).
//
// The plan is only sent to shards whose field facts say the values are what
// the aggregate is exact over, so nothing here checks them: see
// docs/plans/field-types.md, "Keyed sum/min/max".
const (
	ValColumn     = "agg_val"
	TxtColumn     = "agg_txt"
	SumColumn     = "agg_sum"
	BestColumn    = "agg_best"
	BestTxtColumn = "agg_best_txt"
	BestNsColumn  = "agg_best_ns"
)

// SortKeyColumn names the extra column carrying a numeric extreme's number:
// the value sort -n compares the line by.
const SortKeyColumn = "sortkey"

// sortNumberSQL is the number sort -n compares col by, as warp reads it: the
// leading sign, digits and at most one dot after any spaces and tabs, 0 when
// there is no number in them or it doesn't fit a float64. Adding 0.0 turns -0
// into 0, which sort -n treats as equal.
func sortNumberSQL(col string) string {
	k := `TRY_CAST(regexp_extract(` + col + `, '^[ \t]*([+-]?[0-9]*[.]?[0-9]*)', 1) AS DOUBLE)`
	return `CASE WHEN isfinite(` + k + `) THEN ` + k + ` + 0.0 ELSE 0.0 END`
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
// the persisted form; the runtime-context kinds (Partition, TimeStart,
// TimeEnd) get resolved fresh at each fire from the watch's tenant
// and the fire's time window.
type ArgKind int

const (
	// ArgLiteral is a value baked into the pipeline (namespace name,
	// service name, env, etc.). The pipeline can't reference it at
	// fire time — it IS the pipeline.
	ArgLiteral ArgKind = iota
	// ArgCustomer is the row-level-security boundary of a plan stored before
	// the tenant came from the shape: its one value is the customer, the
	// first partition value. Only read; new plans use ArgPartition.
	ArgCustomer
	// ArgTimeStart / ArgTimeEnd are the time window. Resolved from
	// the fire's TimeRange.
	ArgTimeStart
	ArgTimeEnd
	// ArgPartition is one value of the tenant's partition, the one at
	// ArgSource.Part in layout order. Resolved from the watch's stored
	// partition at fire time (or the session's for interactive queries).
	// Appended last so the numbers of the kinds above, which are stored, hold.
	ArgPartition
)

// ArgSource describes one positional argument. Value carries the
// resolved value as of the Plan() that produced this source —
// for ArgLiteral that's the only place the value comes from;
// for ArgPartition/TimeStart/TimeEnd that's the current call's
// resolved value but a save-time-lowering consumer ignores Value
// and re-resolves from fire-time context.
type ArgSource struct {
	Kind  ArgKind
	Value string
	// Part is which partition value an ArgPartition is, in layout order.
	Part int `json:",omitempty"`
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
// etc.). Use addTenant / addTimeBounds for the runtime-resolved
// sources — those tag Args differently so save-time lowering can
// distinguish bake-at-save from resolve-at-fire.
func (q *Query) addPredicate(pred string, literals ...string) {
	args := make([]ArgSource, len(literals))
	for i, v := range literals {
		args[i] = ArgSource{Kind: ArgLiteral, Value: v}
	}
	q.predicates = append(q.predicates, Predicate{SQL: pred, Args: args})
}

// addTenant attaches the row-level security predicate: one equality per
// partition column of the shape, which is none for a shape without a tenant. A
// tenant with no values adds nothing (explain). Tagged ArgPartition so a
// save-time-lowered form resolves each value from the watch's stored
// partition at fire time, not from whatever tenant was active during Plan().
//
// Args travel as strings (see ArgSource), so the placeholder is cast
// explicitly, the same way the timestamp bounds are: it must not depend on
// how DuckDB would coerce a VARCHAR parameter against a typed column.
func (q *Query) addTenant(tn tenant) {
	cols := coords.BuildLayout().Columns
	if len(tn.values) != len(cols) {
		return
	}
	for i, c := range cols {
		q.predicates = append(q.predicates, Predicate{
			SQL:  c.Name + " = CAST(? AS " + c.Type.SQL() + ")",
			Args: []ArgSource{{Kind: ArgPartition, Value: tn.values[i], Part: i}},
		})
	}
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
		rawExpr:        "raw",
		from:           From{Subquery: q, Alias: "t"},
		orderBy:        orderBy,
		metaCols:       q.metaCols,
		includePayload: q.includePayload,
	}
}

// Lines returns q's rows split into the lines compute would feed a stage
// (each row's raw, or q's projection, split on newlines), keeping the
// lines where every pred holds. Each line keeps its row's ts and columns.
// q must have no limit.
func (q *Query) Lines(preds []Predicate) *Query {
	return &Query{
		rawExpr:        "raw",
		from:           From{Subquery: q.lineSplit(), Alias: "t"},
		predicates:     preds,
		metaCols:       q.metaCols,
		includePayload: q.includePayload,
	}
}

// fieldForm returns q selecting, in place of each line, the values of the
// fields spec names and the field count: the row's `fields` column, which
// ingest splits as awk does. A row with no fields keeps its raw only when the
// stage reads the last field, where that is the line itself. q must be the
// plain query on the table (no Lines or Wrap).
func (q *Query) fieldForm(spec *fieldReads) *Query {
	clone := *q
	clone.rawExpr = "''"
	if spec.last {
		clone.rawExpr = "CASE WHEN COALESCE(len(fields), 0) = 0 THEN raw ELSE '' END"
	}
	clone.fieldSpec = spec
	return &clone
}

// Group returns the shard half of a grouping over q's lines, as Lines
// splits and filters them. It selects each line with its CountColumn
// (RenderTotal: one row, an empty line with the total), in the order
// compute merges on. q must have no limit; its meta-variable and payload
// columns are dropped.
//
// agg, for an awk sum, max or min, adds the field's value (see ValColumn) to
// the subquery and what the aggregate reports of it to the group.
func (q *Query) Group(render GroupRender, orderBy string, preds []Predicate, agg *keyedAgg) *Query {
	inner := q.lineSplit()
	inner.metaCols, inner.includePayload = nil, false
	if agg != nil && agg.op != aggCount {
		inner.agg = agg
	}
	return &Query{
		from:       From{Subquery: inner, Alias: "t"},
		predicates: preds,
		orderBy:    orderBy,
		group:      render,
		agg:        inner.agg,
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
// kind (Partition / TimeStart / TimeEnd / Literal) rather than resolved
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
	if q.spanKind != "" {
		q.renderSpans(b, onArg)
		return
	}
	b.WriteString("SELECT ")
	if q.group == RenderNumericMax || q.group == RenderNumericMin {
		// The shard's one best line; compute picks across shards.
		dir := "DESC"
		if q.group == RenderNumericMin {
			dir = "ASC"
		}
		fmt.Fprintf(b, "raw, ts, %s AS %s FROM (", sortNumberSQL("raw"), SortKeyColumn)
		q.from.Subquery.renderInto(b, onArg)
		b.WriteString(") AS " + q.from.Alias)
		q.renderWhere(b, onArg)
		fmt.Fprintf(b, " ORDER BY %s %s, raw %s LIMIT 1", SortKeyColumn, dir, dir)
		return
	}
	if q.group != "" {
		key := "raw"
		if q.group == RenderTotal || q.group == RenderBytes {
			key = "'' AS raw"
		}
		extra := ""
		if q.group == RenderBytes {
			extra = fmt.Sprintf(", CAST(sum(strlen(raw) + 1) AS BIGINT) AS %s", BytesColumn)
		}
		if q.agg != nil {
			extra += q.agg.groupColumns()
		}
		fmt.Fprintf(b, "%s, min(ts) AS ts, count(*) AS %s%s FROM (", key, CountColumn, extra)
		q.from.Subquery.renderInto(b, onArg)
		b.WriteString(") AS " + q.from.Alias)
		q.renderWhere(b, onArg)
		if q.group == RenderTotal || q.group == RenderBytes {
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
	for _, col := range q.metaCols {
		b.WriteString(", ")
		b.WriteString(col)
	}
	if q.includePayload {
		if q.from.Subquery != nil {
			b.WriteString(", " + PayloadColumn)
		} else {
			b.WriteString(", CAST(payload AS VARCHAR) AS " + PayloadColumn)
		}
	}
	if q.agg != nil && q.group == "" {
		b.WriteString(q.agg.valueColumns())
	}
	if q.fieldSpec != nil {
		for _, p := range q.fieldSpec.positions {
			fmt.Fprintf(b, ", list_extract(fields, %d) AS %s%d", p, FieldColumnPrefix, p)
		}
		if q.fieldSpec.last {
			b.WriteString(", list_extract(fields, -1) AS " + FieldLastColumn)
		}
		b.WriteString(", COALESCE(len(fields), 0) AS " + FieldCountColumn)
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

// valueColumns are the columns a keyed aggregate's subquery adds to each line:
// the field's number (a whole number for a sum, exactly), and for a max or min its
// text.
func (a *keyedAgg) valueColumns() string {
	if a.op == aggSum {
		return fmt.Sprintf(", TRY_CAST(list_extract(fields, %d) AS BIGINT) AS %s", a.field, ValColumn)
	}
	return fmt.Sprintf(", TRY_CAST(list_extract(fields, %[1]d) AS DOUBLE) AS %[2]s, list_extract(fields, %[1]d) AS %[3]s", a.field, ValColumn, TxtColumn)
}

// groupColumns are the columns a keyed aggregate's group returns per key
// beyond the count.
func (a *keyedAgg) groupColumns() string {
	if a.op == aggSum {
		return fmt.Sprintf(", COALESCE(CAST(sum(%s) AS VARCHAR), '0') AS %s", ValColumn, SumColumn)
	}
	// The first row, in the order awk meets them, holding the best value: awk
	// replaces only for a strictly better one.
	dir, best := "DESC", "max"
	if a.op == aggMin {
		dir, best = "ASC", "min"
	}
	first := fmt.Sprintf("ORDER BY %s %s, t.ts, t.%s NULLS FIRST, t.%s", ValColumn, dir, EventIDColumn, LineNoColumn)
	return fmt.Sprintf(", %[1]s(%[2]s) AS %[3]s, first(%[4]s %[5]s) AS %[6]s, CAST(epoch_ns(first(t.ts %[5]s)) AS BIGINT) AS %[7]s",
		best, ValColumn, BestColumn, TxtColumn, first, BestTxtColumn, BestNsColumn)
}
