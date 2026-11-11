package planner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"

	"github.com/graybeardcorp/weft/logtable"
	coords "github.com/graybeardcorp/weft/shape"
)

// QueryPlan is the result of analyzing a weft pipeline. ShardSQL is the
// pushed-down SQL sent to shard nodes — always SELECT {raw_expr} as raw, ts.
// The tenant partition + ts >=/<= bounds + namespace/env/service predicates are
// parameterized via positional `?` placeholders; the bound values live
// in Args, in render order. Coordinator ships Args alongside Sql to the
// shard's QueryContext, which forwards to DuckDB's prepared-statement
// path. ComputeStages lists the pipeline stages that must run in the
// compute layer.
type QueryPlan struct {
	ShardSQL string
	Args     []any // positional values for ShardSQL/DirectShardSQL placeholders, in render order
	// ArgSources is the same args slice but each entry is tagged with its
	// Kind (Partition, TimeStart, TimeEnd, Literal). Live dispatch ignores
	// this; save-time lowering reads it so each fire can re-resolve the
	// runtime kinds from fresh context. 1:1 with Args.
	ArgSources    []ArgSource
	ComputeStages []string
	// RowStages are stages that pass rows through unchanged (filters,
	// head/tail -n N), which the compute engine runs per merged row,
	// after Limit and before ComputeStages, because a later stage reads
	// row metadata. A jq select chain reads the payload; grep, sed and
	// awk read the raw line (awk with its meta-variables). See
	// docs/plans/planner-ir.md, "Row identity".
	RowStages []string
	// StagesReadPayload makes the first ComputeStage, a jq, read each
	// row's payload instead of its raw line. Only set with RowStages;
	// without them SQL selects the payload as raw instead.
	StagesReadPayload bool
	Limit             int // post-merge limit (from head/tail), 0 = unlimited
	// TailMode is true when the source command is `tail` (or an inline
	// `tail` stage). Coordinator forwards it to compute as KeepLast,
	// which switches the merged-stream trim from "stop after N emitted"
	// (head semantics) to "retain last N, emit at EOF" (tail semantics)
	// — required because the tail subquery flip lands rows in ts ASC at
	// compute, so the global last N can't be resolved by stopping early.
	TailMode       bool
	Follow         bool          // tail -f: keep stream open for live updates
	FollowPatterns []GrepPattern // grep filters applied to live rows
	// LiveStages is the post-source stage list to re-run per live row in
	// follow mode. Same strings as ComputeStages would have been if we
	// hadn't pushed anything down — i.e., a faithful reconstruction of
	// the user's pipeline minus the source. Consumed by
	// coordinator.BuildLiveFilter so the live broker rows go through
	// the same transformations as the historical batch.
	LiveStages []string
	// LiveRowStages and LiveReadsPayload are RowStages and the
	// payload rule for the live path: leading row stages (removed from
	// LiveStages) keep or drop each live row, and a jq first LiveStage
	// reads the row's payload instead of its raw line.
	LiveRowStages    []string
	LiveReadsPayload bool
	SourceCmd        string // source command: "cat", "head", "tail", or "trace"
	// SourcePartition is the partition the query is scoped to, as text in the
	// shape's layout order; nil when the plan carries no tenant (explain).
	SourcePartition []string
	SourceNamespace string // namespace column value — routing ("" = all namespaces)
	// Coords is what the query reads: the source's tuples intersected with the
	// scope, pinned before any pass runs. The SQL, shard routing, table choice
	// and the live matcher are all backends over it.
	Coords        coords.Set
	SourceService string   // service column value — routing ("" = all services in the namespace)
	SourceKind    string   // kind column value — live-tail matching ("" = all kinds)
	AlertArgs     []string // argv for a terminal `alert` stage; nil if absent. Set even for alert-only pipelines (ShardSQL is empty in that case).

	// Table names the shard table that holds the source's rows, when the source
	// pins exactly one (partition, namespace, service, kind). "" = no hint: the
	// predicates alone pick the rows. See sourceTable.
	Table string

	// Direct-to-compute mode (see docs/plans/direct-to-compute.md §3).
	// Populated by sort/limit fusion (passes.go) from the compute stages'
	// terminal sort. Consumed by compute's merge_key and by the
	// coordinator when taking the direct-to-compute path.
	ShardOrderBy string // "", "ts[ DESC]", or "raw[ DESC]"
	ShardLimit   int    // per-shard LIMIT; 0 when pushdown is unsafe

	// Group, when set, says the shard SQL groups the rows (wc -l, sort -u,
	// sort | uniq [-c]): each shard row is a distinct line with its
	// CountColumn, merged on ShardOrderBy. Compute sums the counts across
	// shards and prints the grouped stages' output, which ComputeStages
	// then read.
	Group *GroupSpec

	// Typed, when set, is the shard SQL for a shard whose field facts say
	// the locators it needs are all numbers; see TypedPlan.
	Typed *TypedPlan

	// FieldSQL and FieldSpec are, for a first compute stage that reads only
	// some of each row's fields, the field form of DirectShardSQL: the same
	// query returning those fields' values (see Query.fieldForm) instead of a
	// line built from them. A shard that has it runs FieldSQL, compute
	// rebuilds the line from FieldSpec, and a shard that doesn't runs
	// DirectShardSQL, whose line carries the same fields. Both empty when the
	// stage reads more, or the query has no field form.
	FieldSQL  string
	FieldSpec *FieldSpec

	// Aggregate is the trace and span queries' shard-side aggregate (one
	// of the AggregateSpan* / AggregateTrace* constants), which compute
	// finishes the same way.
	Aggregate string

	// TraceStats, when set, is the second step of the trace list: the
	// coordinator runs ShardSQL (Aggregate = AggregateTraceIDs) to find the
	// newest trace IDs, then this query for exactly those traces.
	TraceStats *TraceStatsQuery

	// DirectShardSQL is the SQL compute sends to shards on the
	// coordinator's behalf — same body as ShardSQL but with ShardOrderBy +
	// ShardLimit substituted for the head/tail-derived "ts ASC"/
	// "ts DESC". Kept separate so callers still using the legacy Query
	// RPC (no compute stages, backward-compat tests) see the old
	// behavior in ShardSQL.
	DirectShardSQL string

	// RedirectTarget is the destination of a trailing `> /tmp/...`
	// (or other shellfs namespace) redirect on the source-query
	// pipeline. Empty means no redirect. Coordinators write the
	// rendered query result to this path via shellfs after executing
	// the rest of the pipeline.
	RedirectTarget string
	// RedirectAppend distinguishes `>>` (append) from `>` (overwrite).
	RedirectAppend bool
}

// GroupRender is what compute prints from a group's summed counts.
type GroupRender string

const (
	// RenderTotal is `wc -l`: one bare total of every count.
	RenderTotal GroupRender = "total"
	// RenderLine is `sort -u` or `sort | uniq`: each line once.
	RenderLine GroupRender = "line"
	// RenderCountLine is `sort | uniq -c`: each line with its count, as
	// uniq -c prints it.
	RenderCountLine GroupRender = "count_line"
	// RenderDistinctTotal is one bare number, how many distinct lines there
	// are: `sort -u | wc -l`.
	RenderDistinctTotal GroupRender = "distinct_total"
	// RenderKeyCount is each distinct line, a space, and its count: an awk
	// tally.
	RenderKeyCount GroupRender = "key_count"
	// RenderKeySum, RenderKeyMax and RenderKeyMin are each distinct key, a
	// space, and the sum, greatest or least of a field over its rows: an awk
	// aggregate (see keyedAgg). The shard sends the value beside the key, and
	// compute merges the shards' values.
	RenderKeySum GroupRender = "key_sum"
	RenderKeyMax GroupRender = "key_max"
	RenderKeyMin GroupRender = "key_min"
	// RenderBytes is one bare number, the bytes of every line with a
	// newline after each: `wc -c`.
	RenderBytes GroupRender = "bytes"
	// RenderNumericMax is the one line `sort -n | tail -n 1` (or `sort -rn |
	// head -n 1`) prints: the greatest leading number, ties to the greater
	// line. RenderNumericMin is the other end.
	RenderNumericMax GroupRender = "numeric_max"
	RenderNumericMin GroupRender = "numeric_min"
)

// ValueOrder is how compute orders the lines of an awk aggregate, `key value`,
// before printing them: `sort -k2 -n` and its relatives (see sortArgs), applied
// to the printed lines as warp's sort applies it. Reverse reverses the key's
// order; TieReverse reverses the comparison of whole lines that settles ties,
// which `sort -k2,2nr` does not and `sort -r -n -k2` does.
type ValueOrder struct {
	Reverse    bool `json:"reverse,omitempty"`
	TieReverse bool `json:"tie_reverse,omitempty"`
}

// GroupSpec is the shard-side grouping a plan asks for (QueryPlan.Group).
type GroupSpec struct {
	Render GroupRender `json:"render"`
	// Order, for an awk aggregate, orders its lines by value, and Limit keeps
	// the first of them (`sort -k2 -rn | head -n N`).
	Order *ValueOrder `json:"order,omitempty"`
	// ByCount orders the groups highest count first (count_line only),
	// and Limit keeps that many of them (0 keeps all): `sort -rn | head
	// -n N` after uniq -c.
	ByCount bool `json:"by_count,omitempty"`
	Limit   int  `json:"limit,omitempty"`
}

// GrepPattern captures a grep filter for live tail matching.
type GrepPattern struct {
	Pattern    string
	IgnoreCase bool
	Invert     bool
}

// ErrNotPlannable is the typed sentinel Plan returns when a pipeline
// has no /log/... source — i.e. it isn't something the planner can
// turn into shard SQL. The coordinator's rewriter classifies log
// pipelines lexically (see query_rewrite.go), so this error mostly
// surfaces from the rewriter callback re-planning a substituted
// pipeline whose shape unexpectedly changed.
var ErrNotPlannable = errors.New("not a plannable log query")

// Plan plans a pipeline for a principal who may read only scope. The query
// reads the intersection of what it names and scope, and scope names the one
// partition (tenant) the query is for: a scope whose tuples name different
// partitions is an error. A wildcard (warp.log, a glob, an unnamed namespace)
// means every tuple in scope, never "no predicate", and an empty intersection
// is ErrNoSource before anything is routed, which is also what an empty scope
// gets. A trace that names no namespace is restricted to scope, because its
// rows, counts and per-trace service lists come only from the SQL.
func Plan(pipeline string, scope coords.Set, cwd string, start, end time.Time) (*QueryPlan, error) {
	plan, _, err := compile(pipeline, scope, cwd, start, end)
	return plan, err
}

// ErrNoSource is the deny for a query whose source, intersected with what the
// principal may read, is empty. It never says whether the source exists.
var ErrNoSource = errors.New("unknown source")

// noSource is ErrNoSource naming the first namespace the user wrote, which is
// their own input, so the refusal reads the same for a namespace that is
// missing and one they may not read.
func noSource(named coords.Set) error {
	for _, t := range named {
		if ns, ok := t.Namespace.Literal(); ok {
			return fmt.Errorf("%w: %s", ErrNoSource, ns)
		}
	}
	return ErrNoSource
}

// compile plans a pipeline and also returns its IR, which Explain shows.
// The IR is nil for an alert-only pipeline. scope also scopes trace commands
// (see Plan).
func compile(pipeline string, scope coords.Set, cwd string, start, end time.Time) (*QueryPlan, *compiled, error) {
	tn, err := tenantOf(scope)
	if err != nil {
		return nil, nil, err
	}
	return compileTenant(pipeline, tn, scope, cwd, start, end)
}

// compileBare plans with no tenant and no scope limit: how Explain and analyze
// show a pipeline. The plan it returns reads nothing for anyone.
func compileBare(pipeline, cwd string, start, end time.Time) (*QueryPlan, *compiled, error) {
	return compileTenant(pipeline, tenant{}, coords.Set{coords.Coord{}}, cwd, start, end)
}

func compileTenant(pipeline string, tn tenant, scope coords.Set, cwd string, start, end time.Time) (*QueryPlan, *compiled, error) {
	plan, c, err := compileFor(pipeline, tn, cwd, start, end, scope, false)
	if err != nil || plan.Typed == nil {
		return plan, c, err
	}
	// The same pipeline planned for shards that have said every field it
	// compares is a finite decimal: the comparison is then exact, so SQL can
	// compute all of the stage and compute has less (or nothing) to run.
	exact, _, err := compileFor(pipeline, tn, cwd, start, end, scope, true)
	if err != nil {
		return nil, nil, err
	}
	plan.Typed.Exact = exact
	return plan, c, nil
}

// compileFor is compile for one reading of the facts: assumeTyped says every
// field the pipeline compares with a number holds a finite decimal.
func compileFor(pipeline string, tn tenant, cwd string, start, end time.Time, scope coords.Set, assumeTyped bool) (*QueryPlan, *compiled, error) {
	queryPart, alertArgs := SplitAlertStage(pipeline)

	// Alert-only pipeline (`alert ...` with nothing in front). Return
	// a plan that carries only the alert args; the empty ShardSQL is
	// the caller's signal to short-circuit shard execution.
	if queryPart == "" && alertArgs != nil {
		return &QueryPlan{AlertArgs: alertArgs}, nil, nil
	}

	// Strip a trailing `> /tmp/...` (or other shellfs namespace)
	// redirect off the query pipeline before classification. Without
	// this the redirect would be silently dropped by executeQuery —
	// the result rows never reach FS write machinery. We capture the
	// target and rebuild queryPart from the surviving AST.
	queryPart, redirTarget, redirAppend, err := extractTrailingFSRedirect(queryPart)
	if err != nil {
		return nil, nil, fmt.Errorf("plan: %w", err)
	}

	info, err := parsePipeline(queryPart, cwd)
	if err != nil {
		// Not a log query — non-/log/ source path or an unknown
		// source command. Caller (Coordinator.Execute) routes these
		// to warp via runLocal; ErrNotPlannable is the typed signal.
		if strings.Contains(err.Error(), "unknown source command") || errors.Is(err, errLocalSourcePath) {
			return nil, nil, ErrNotPlannable
		}
		return nil, nil, fmt.Errorf("plan: %w", err)
	}

	// Pin the coordinates: what the source names, held to what the scope allows.
	named := info.source.tuples
	if len(named) == 0 {
		named = coords.Set{{}} // a trace that names no namespace
	}
	bound := make(coords.Set, len(named))
	partition := tn.coord()
	for i, t := range named {
		bound[i] = t.WithPartition(partition)
	}
	info.source.coord = coords.Intersect(bound, scope)
	// A tuple the scope leaves nothing of is a refusal, not something to
	// drop: the rest would run and the answer would silently miss it.
	for _, t := range bound {
		if len(info.source.coord) > 0 && len(coords.Intersect(coords.Set{t}, scope)) == 0 {
			return nil, nil, noSource(coords.Set{t})
		}
	}
	if len(info.source.coord) == 0 {
		// Nothing readable selects nothing for a trace listing; every other
		// source is refused.
		if strings.HasPrefix(info.source.cmd, "trace") && len(info.source.tuples) == 0 {
			return &QueryPlan{SourceCmd: info.source.cmd, AlertArgs: alertArgs}, &compiled{source: info.source}, nil
		}
		return nil, nil, noSource(bound)
	}
	info.source.scope = scope

	if info.source.follow && redirTarget != "" {
		return nil, nil, fmt.Errorf("plan: %w", FollowRedirectError(redirTarget))
	}

	// Frontends: every stage lowers to IR nodes, after the source's own
	// head/tail line count.
	c := &compiled{source: info.source}
	switch info.source.cmd {
	case "head":
		c.nodes = append(c.nodes, &limitNode{n: info.source.limit, order: "ts ASC"})
	case "tail":
		c.nodes = append(c.nodes, &limitNode{n: info.source.limit, order: "ts DESC"})
	}
	for i, st := range info.stages {
		ls := lowerStage(st, i+1, assumeTyped)
		c.stages = append(c.stages, ls)
		c.nodes = append(c.nodes, ls.nodes...)
	}

	if agg := spanAggregateOf(info.source); agg != "" {
		return planSpans(c, info.source, agg, tn, start, end, alertArgs, redirTarget, redirAppend)
	}

	// Passes. Aggregate recognition and sort/limit fusion run below,
	// once the SQL holds the pushed line count.
	c.pushFilters()
	c.splitCompute()
	c.pushProjection()
	c.hoistFilters()
	// Shard rows carry meta-variables to the row stages, which run on them, and
	// to the first compute stage; a later stage reads what the one before it
	// wrote.
	for _, s := range append(slices.Clone(c.rowStages), c.compute[:min(len(c.compute), 1)]...) {
		c.meta = c.meta.merge(s.meta)
	}

	// SQL backend: the source, then every node the shard runs.
	sql := newSQLBuilder()
	// Tenant + time bounds first so they sit at the head of the
	// WHERE clause regardless of subsequent app/source/stage pushdowns.
	// addTenant/addTimeBounds are no-ops on empty/zero values.
	sql.addTenant(tn)
	sql.addTimeBounds(start, end)
	addSourcePredicates(sql, info.source)

	var grepPatterns []GrepPattern // collected for follow mode
	var linePreds []Predicate      // hoisted filters, on each line
	for _, n := range c.nodes[:c.shard] {
		switch n := n.(type) {
		case *filterNode:
			if n.hoisted {
				linePreds = append(linePreds, Predicate{SQL: n.pred})
				continue
			}
			sql.addPredicate(n.pred)
			if n.follow != nil {
				grepPatterns = append(grepPatterns, *n.follow)
			}
		case *limitNode:
			if n.order != "" {
				sql.setOrderLimit(n.order, n.n)
			} else {
				sql.setLimitOffset(n.n, n.offset)
			}
		}
	}

	switch {
	case c.selected != nil:
		sql.setSelect(c.selected.expr)
	case len(c.rowStages) > 0:
		sql.includePayload = c.readsPayload
		for _, rs := range c.rowStages {
			sql.includePayload = sql.includePayload || rs.cmd == "jq"
		}
	case c.readsPayload:
		sql.setSelect("CAST(payload AS VARCHAR)")
	}
	// A compute stage reading a meta-variable (`jq -r '$host'`, awk's
	// `HOST`) gets it from the row: the shard projects every
	// MetaVarColumns column alongside raw, and compute (compute/
	// metavars.go) hands the values to the stage.
	if c.meta.Reads() {
		sql.metaCols = c.meta.Columns()
	}

	orderDesc := sql.orderBy == "ts DESC"
	c.fuseAggregate(sql.limit > 0 || sql.offset > 0)
	if c.groupKey != "" {
		sql.setSelect(c.groupKey)
	}
	if c.group == nil {
		c.projectFields(sql.limit > 0 || sql.offset > 0)
		c.projectPayload()
		if c.payloadSelect != "" {
			sql.setSelect(c.payloadSelect)
		}
	}
	if c.aggregate == "" && c.group == nil {
		c.fuseSort(orderDesc, sql.limit)
	}

	// tail wraps its inner DESC + LIMIT pushdown in an outer ASC re-emit
	// so the user-visible rows arrive in chronological order. The inner
	// keeps doing the cheap per-shard top-N work; the outer flip is a
	// bounded re-sort. ShardSQL preserves the head/tail-derived inner
	// ordering verbatim, so we wrap iff the inner is "ts DESC". The
	// DirectShardSQL path's inner uses the order from sort/limit fusion
	// — wrap whenever that's "ts DESC" (Mode A with tail). Other DESC
	// keys (raw DESC from sort -r) are explicit user intent and not
	// wrapped.
	// Hoisted filters keep lines, so the shard returns those; a pushed
	// line count declines hoisting, so there's no ts DESC to wrap.
	rows := sql
	if len(linePreds) > 0 {
		rows = sql.Lines(linePreds)
	}
	shardQuery := rows
	if sql.orderBy == "ts DESC" {
		shardQuery = sql.Wrap("ts ASC")
	}
	shardSQL, shardArgs, shardArgSources := shardQuery.ToSQLWithSources()

	directInner := rows.WithOrderLimit(c.shardOrderBy, c.shardLimit)
	directQuery := directInner
	if c.shardOrderBy == "ts DESC" {
		directQuery = directInner.Wrap("ts ASC")
	}
	if c.group != nil {
		// No line count to keep apart: both paths group.
		shardQuery = sql.Group(c.group.Render, c.shardOrderBy, linePreds, c.groupAgg)
		directQuery = shardQuery
		shardSQL, shardArgs, shardArgSources = shardQuery.ToSQLWithSources()
	}
	directShardSQL, _ := directQuery.ToSQL()
	// ShardSQL and DirectShardSQL render identical WHERE clauses (only
	// order/limit differ), so their args slices match — Args lives once
	// on the plan rather than duplicating per build path.

	// After the wrap, rows reach compute in ts ASC even though the
	// inner pushdown was DESC. Reflect that in the merge-key the
	// coordinator forwards to compute, otherwise the k-way merge
	// walks the per-shard streams in the wrong direction.
	mergeOrderBy := c.shardOrderBy
	if c.shardOrderBy == "ts DESC" {
		mergeOrderBy = "ts"
	}

	// Live tail re-runs the user's post-source stages, verbatim, on each
	// live row, so its rows go through the same transformations as the
	// historical batch. Leading row stages keep or drop the row, and a
	// jq after them reads its payload; there's no SQL projection to fold
	// into on the live path.
	liveRows, liveRest := splitRowStages(c.stages)
	liveStages := stageTexts(c.stages)
	if len(liveRows) > 0 {
		liveStages = stageTexts(liveRest)
	}

	// The field form of the query, where the stage's rows are the table's
	// (no line split, no pushed line count): same WHERE and order, with the
	// fields as columns.
	var fieldSQL string
	var fieldSpec *FieldSpec
	if c.fieldSpec != nil && len(linePreds) == 0 {
		fieldSQL, _ = sql.fieldForm(c.fieldSpec).WithOrderLimit(c.shardOrderBy, c.shardLimit).ToSQL()
		fieldSpec = c.fieldSpec.spec()
	}

	typed := typedVariant(c, info.source, tn, shardSQL, directShardSQL, fieldSQL)
	if assumeTyped {
		// What the facts leave to swap (a jq key) is swapped in this plan,
		// which is only ever used where they all hold.
		if typed != nil {
			shardSQL, directShardSQL, fieldSQL = typed.ShardSQL, typed.DirectShardSQL, typed.FieldSQL
		}
		typed = nil
	}
	return &QueryPlan{
		Typed:             typed,
		FieldSQL:          fieldSQL,
		FieldSpec:         fieldSpec,
		ShardSQL:          shardSQL,
		Args:              shardArgs,
		ArgSources:        shardArgSources,
		DirectShardSQL:    directShardSQL,
		ComputeStages:     stageTexts(c.compute),
		RowStages:         stageTexts(c.rowStages),
		StagesReadPayload: len(c.rowStages) > 0 && c.readsPayload,
		Limit:             sql.limit,
		TailMode:          orderDesc,
		Follow:            info.source.follow,
		FollowPatterns:    grepPatterns,
		LiveStages:        liveStages,
		LiveRowStages:     stageTexts(liveRows),
		LiveReadsPayload:  len(liveRest) > 0 && liveRest[0].cmd == "jq",
		SourceCmd:         info.source.cmd,
		SourcePartition:   tn.values,
		SourceNamespace:   routeNamespace(info.source),
		Coords:            info.source.coord,
		SourceService:     routeService(info.source),
		SourceKind:        routeKind(info.source),
		Table:             sourceTable(tn, info.source),
		AlertArgs:         alertArgs,
		ShardOrderBy:      mergeOrderBy,
		ShardLimit:        c.shardLimit,
		Group:             c.group,
		Aggregate:         c.aggregate,
		RedirectTarget:    redirTarget,
		RedirectAppend:    redirAppend,
	}, c, nil
}

// sourceTable names the shard table that holds a cat, head or tail source's
// rows, or "" when the source does not pin exactly one (partition, namespace,
// service, kind): several files or a glob, or a trace. The env is a column and
// stays a predicate, so it is not part of the name. The partition comes from
// the authenticated query, never from the pipeline.
func sourceTable(tn tenant, src sourceInfo) string {
	switch src.cmd {
	case "cat", "head", "tail":
	default:
		return ""
	}
	ns, svc, kind, ok := src.single()
	if !tn.complete() || !ok {
		return ""
	}
	return logtable.Name(tn.typed, ns, svc, kind)
}

// single is the one (namespace, service, kind) the pinned coordinates name, when
// they are exactly one tuple that pins all three; env is a column, not part of
// a table.
func (s sourceInfo) single() (ns, svc, kind string, ok bool) {
	if len(s.coord) != 1 {
		return "", "", "", false
	}
	t := s.coord[0]
	ns, ok1 := t.Namespace.Literal()
	svc, ok2 := t.Service.Literal()
	kind, ok3 := t.Kind.Literal()
	return ns, svc, kind, ok1 && ok2 && ok3
}

// addSourcePredicates applies the source's filters: each pseudofile
// path segment maps to a column (namespace, env, service, kind), and a
// trace source to its trace or span columns. The path values are
// customer-supplied identifiers, so they go through positional
// placeholders rather than string interpolation — defense in depth
// against an adversarial namespace name even though the coordinator's
// CustomerHasSource allowlist gates the source up front.
func addSourcePredicates(sql *Query, src sourceInfo) {
	addCoordPredicates(sql, src.coord)

	if src.cmd == "trace" && src.traceID != "" {
		sql.addPredicate("trace_id = ?", src.traceID)
	}
	if src.cmd == "trace_list" {
		sql.addPredicate("trace_id IS NOT NULL")
		sql.addPredicate("trace_id <> ''")
	}
	if src.cmd == "trace_spans" || (src.cmd == "trace" && src.spansOnly) {
		sql.addPredicate("span_name IS NOT NULL")
		if src.samples {
			sql.addPredicate("duration_ns > 0")
		} else {
			sql.addPredicate("(duration_ns > 0 OR level = 'ERROR')")
		}
	}
	if src.cmd == "trace_spans" && src.spanName != "" {
		sql.addPredicate("span_name = ?", src.spanName)
	}
	if (src.cmd == "trace" || src.cmd == "trace_spans") && spanAggregateOf(src) == "" {
		// A trace fans out across every service a request touched, so its
		// rows always carry namespace and service.
		sql.metaCols = MetaVarColumns
	}
}

// colOf is a coordinate column as the SQL names it.
type colOf struct {
	name string
	cell func(coords.Coord) coords.Cell
}

var coordCols = []colOf{
	{"env", func(c coords.Coord) coords.Cell { return c.Env }},
	{"namespace", func(c coords.Coord) coords.Cell { return c.Namespace }},
	{"service", func(c coords.Coord) coords.Cell { return c.Service }},
	{"kind", func(c coords.Coord) coords.Cell { return c.Kind }},
}

// literal is the SQL value a constrained cell compares to. Null is the empty
// string, which is how storage records a missing value.
func literal(c coords.Cell) (string, bool) {
	if v, ok := c.Literal(); ok {
		return v, true
	}
	return "", !c.IsAny()
}

// addCoordPredicates is the SQL backend over the pinned coordinates, and it is
// exact. When the tuples are a per-column product it renders each column on
// its own (`namespace IN (...)`), which reads the same rows and stays compact;
// otherwise it renders the union of tuples. A tuple that constrains nothing
// admits every row, so the union adds no predicate.
func addCoordPredicates(sql *Query, set coords.Set) {
	if len(set) == 0 {
		return
	}
	if isProduct(set) {
		for _, col := range coordCols {
			var vals []string
			seen := map[string]bool{}
			for _, t := range set {
				if v, ok := literal(col.cell(t)); ok && !seen[v] {
					seen[v] = true
					vals = append(vals, v)
				}
			}
			slices.Sort(vals)
			addInPredicate(sql, col.name, vals)
		}
		return
	}
	exact, args, hasEnv := unionOf(set, coordCols)
	if exact == "" {
		return
	}
	if hasEnv {
		// env is not part of an archive's key directories, so a branch that
		// names it hides the key columns from hive pruning and every key
		// directory gets opened. The same union over the key columns alone is a
		// superset of the exact one: it adds no rows, and lets DuckDB skip the
		// directories no branch can match.
		if keys, keyArgs, _ := unionOf(set, coordCols[1:]); keys != "" {
			sql.addPredicate(keys, keyArgs...)
		}
	}
	sql.addPredicate(exact, args...)
}

// unionOf renders the tuples as a union over cols: each tuple's constrained
// columns ANDed, the tuples ORed, repeated branches dropped. It is "" when some
// tuple constrains none of cols, since that tuple admits every row. hasEnv
// reports whether any branch constrains env.
func unionOf(set coords.Set, cols []colOf) (sql string, args []string, hasEnv bool) {
	var groups []string
	seen := map[string]bool{}
	for _, t := range set {
		var conj, vals []string
		for _, col := range cols {
			if v, ok := literal(col.cell(t)); ok {
				conj = append(conj, col.name+" = ?")
				vals = append(vals, v)
				hasEnv = hasEnv || col.name == "env"
			}
		}
		if len(conj) == 0 {
			return "", nil, false
		}
		g := "(" + strings.Join(conj, " AND ") + ")"
		if key := g + "\x00" + strings.Join(vals, "\x00"); !seen[key] {
			seen[key] = true
			groups = append(groups, g)
			args = append(args, vals...)
		}
	}
	return "(" + strings.Join(groups, " OR ") + ")", args, hasEnv
}

// isProduct reports whether the tuples are exactly the per-column product of
// their values, with each column either constrained in every tuple or in none.
// Only then do per-column lists read the same rows as the tuples.
func isProduct(set coords.Set) bool {
	size := 1
	for _, col := range coordCols {
		vals := map[string]bool{}
		constrained := 0
		for _, t := range set {
			if v, ok := literal(col.cell(t)); ok {
				constrained++
				vals[v] = true
			}
		}
		if constrained != 0 && constrained != len(set) {
			return false
		}
		if constrained != 0 {
			size *= len(vals)
		}
	}
	distinct := set[:0:0]
	for _, t := range set {
		if !slices.Contains(distinct, t) {
			distinct = append(distinct, t)
		}
	}
	return size == len(distinct)
}

// addInPredicate filters col to values: nothing for none (warp.log, or
// a namespace-level wildcard), = for one, IN for several.
func addInPredicate(sql *Query, col string, values []string) {
	switch len(values) {
	case 0:
	case 1:
		sql.addPredicate(col+" = ?", values[0])
	default:
		placeholders := strings.Repeat("?, ", len(values)-1) + "?"
		sql.addPredicate(col+" IN ("+placeholders+")", values...)
	}
}

// FollowRedirectError is the refusal for `tail -f ... > target`: follow
// never finishes, so the target would hold a truncated finite read. Shared
// by Plan and the coordinator's redirect-lifting rewriter so the wording
// lives in one place.
func FollowRedirectError(target string) error {
	return fmt.Errorf("tail -f can't be redirected to %s: follow never finishes, so the file would hold a truncated read; drop -f to save a finished query", target)
}

// SourceFollows reports whether pipeline's source command is `tail -f`.
func SourceFollows(pipeline, cwd string) bool {
	info, err := parsePipeline(pipeline, cwd)
	return err == nil && info.source.follow
}

// routeNamespace and its siblings are the superset backend over the pinned
// coordinates: shards are narrowed by a value every tuple shares, and by
// nothing otherwise, so a read of several namespaces searches every shard
// rather than only the first namespace's.
func routeNamespace(src sourceInfo) string {
	v, _ := coords.Shared(src.coord, func(c coords.Coord) coords.Cell { return c.Namespace })
	return v
}

func routeService(src sourceInfo) string {
	v, _ := coords.Shared(src.coord, func(c coords.Coord) coords.Cell { return c.Service })
	return v
}

func routeKind(src sourceInfo) string {
	v, _ := coords.Shared(src.coord, func(c coords.Coord) coords.Cell { return c.Kind })
	return v
}

// extractTrailingFSRedirect detects a `> /tmp/...` (or `>>`, or any
// other non-log shellfs namespace) redirect on the last command of
// the leading pipeline. When found, it returns the pipeline string
// rebuilt without the redirect plus the captured target/append flag.
// Redirects that don't address a known shellfs namespace are left in
// place — they belong to a downstream stage's normal exec semantics
// and the planner has nothing useful to add. Multiple redirects on
// the same command are handled by stripping the last fs-targeted one
// and leaving the rest intact.
func extractTrailingFSRedirect(pipeline string) (string, string, bool, error) {
	if pipeline == "" {
		return pipeline, "", false, nil
	}
	script, err := bash.Parse(pipeline)
	if err != nil {
		// Let parsePipeline surface the parse error in its usual shape.
		return pipeline, "", false, nil
	}
	if len(script.Commands) == 0 || script.Commands[0] == nil || script.Commands[0].First == nil {
		return pipeline, "", false, nil
	}
	pipe := script.Commands[0].First
	if len(pipe.Commands) == 0 {
		return pipeline, "", false, nil
	}
	last, ok := pipe.Commands[len(pipe.Commands)-1].(*bash.SimpleCommand)
	if !ok || len(last.Redirects) == 0 {
		return pipeline, "", false, nil
	}
	// Walk the redirects right-to-left so we strip the *last* fs-bound
	// one (matching shell precedence: `> /tmp/a > /tmp/b` lands the
	// final write at /tmp/b).
	for i := len(last.Redirects) - 1; i >= 0; i-- {
		r := last.Redirects[i]
		if r.Op != ">" && r.Op != ">>" {
			continue
		}
		target := r.Target.Literal()
		if !IsLocalFSPath(target) {
			continue
		}
		isAppend := r.Op == ">>"
		last.Redirects = append(last.Redirects[:i], last.Redirects[i+1:]...)
		return bash.Unparse(script), target, isAppend, nil
	}
	return pipeline, "", false, nil
}

// stageTexts returns each stage's own source text.
func stageTexts(stages []*loweredStage) []string {
	var out []string
	for _, s := range stages {
		out = append(out, s.src)
	}
	return out
}
