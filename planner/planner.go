package planner

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// QueryPlan is the result of analyzing a weft pipeline. ShardSQL is the
// pushed-down SQL sent to shard nodes — always SELECT {raw_expr} as raw, ts.
// The customer + ts >=/<= bounds + namespace/env/service predicates are
// parameterized via positional `?` placeholders; the bound values live
// in Args, in render order. Coordinator ships Args alongside Sql to the
// shard's QueryContext, which forwards to DuckDB's prepared-statement
// path. ComputeStages lists the pipeline stages that must run in the
// compute layer.
type QueryPlan struct {
	ShardSQL string
	Args     []any // positional values for ShardSQL/DirectShardSQL placeholders, in render order
	// ArgSources is the same args slice but each entry is tagged with its
	// Kind (Customer, TimeStart, TimeEnd, Literal). Live dispatch ignores
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
	SourceCmd        string   // source command: "cat", "head", "tail", or "trace"
	SourceCustomerID int64    // the customer_id the query is scoped to (0 only in tests)
	SourceNamespace  string   // namespace column value — routing + allowlist ("" = all namespaces)
	SourceService    string   // service column value — routing ("" = all services in the namespace)
	SourceKind       string   // kind column value — live-tail matching ("" = all kinds)
	AlertArgs        []string // argv for a terminal `alert` stage; nil if absent. Set even for alert-only pipelines (ShardSQL is empty in that case).

	// Direct-to-compute mode (see docs/plans/direct-to-compute.md §3).
	// Populated by sort/limit fusion (passes.go) from the compute stages'
	// terminal sort. Consumed by compute's merge_key and by the
	// coordinator when taking the direct-to-compute path.
	ShardOrderBy string // "", "ts[ DESC]", or "raw[ DESC]"
	ShardLimit   int    // per-shard LIMIT; 0 when pushdown is unsafe

	// Aggregate, when set, says the shard SQL groups the rows (wc -l,
	// sort -u, sort | uniq [-c]): each shard row is a distinct line with
	// its CountColumn, merged on ShardOrderBy. Compute sums the counts
	// across shards and prints the aggregated stages' output, which
	// ComputeStages then read. One of the Aggregate* constants.
	Aggregate string

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

// The aggregates a shard can compute (QueryPlan.Aggregate), named for
// what compute prints from the summed counts.
const (
	// AggregateLines is `wc -l`: the total.
	AggregateLines = "lines"
	// AggregateDistinct is `sort -u` or `sort | uniq`: each line once.
	AggregateDistinct = "distinct"
	// AggregateCountDistinct is `sort | uniq -c`: each line with its
	// count, as uniq -c prints it.
	AggregateCountDistinct = "count_distinct"
)

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

func Plan(pipeline string, customerID int64, cwd string, start, end time.Time) (*QueryPlan, error) {
	plan, _, err := compile(pipeline, customerID, cwd, start, end)
	return plan, err
}

// compile plans a pipeline and also returns its IR, which Explain shows.
// The IR is nil for an alert-only pipeline.
func compile(pipeline string, customerID int64, cwd string, start, end time.Time) (*QueryPlan, *compiled, error) {
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
		ls := lowerStage(st, i+1)
		c.stages = append(c.stages, ls)
		c.nodes = append(c.nodes, ls.nodes...)
	}

	// Passes. Aggregate recognition and sort/limit fusion run below,
	// once the SQL holds the pushed line count.
	c.pushFilters()
	c.splitCompute()
	c.pushProjection()
	c.hoistFilters()
	for _, s := range append(c.rowStages, c.compute...) {
		c.readsMeta = c.readsMeta || s.readsMeta
	}

	// SQL backend: the source, then every node the shard runs.
	sql := newSQLBuilder()
	// Customer + time bounds first so they sit at the head of the
	// WHERE clause regardless of subsequent app/source/stage pushdowns.
	// addCustomer/addTimeBounds are no-ops on empty/zero values.
	sql.addCustomer(customerID)
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
	// A compute stage reading a meta-variable (`jq -r '._host'`, awk's
	// `HOST`) gets it from the row: the shard projects every
	// MetaVarColumns column alongside raw, and compute (compute/
	// metavars.go) hands the values to the stage.
	if c.readsMeta {
		sql.includeMetaVars = true
	}

	orderDesc := sql.orderBy == "ts DESC"
	c.fuseAggregate(sql.limit > 0 || sql.offset > 0)
	if c.aggregate == "" {
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
	if c.aggregate != "" {
		// No line count to keep apart: both paths group.
		shardQuery = sql.Aggregate(c.aggregate, c.shardOrderBy, linePreds)
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

	return &QueryPlan{
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
		SourceCustomerID:  customerID,
		SourceNamespace:   firstOrEmpty(info.source.namespaces),
		SourceService:     firstOrEmpty(info.source.services),
		SourceKind:        firstOrEmpty(info.source.kinds),
		AlertArgs:         alertArgs,
		ShardOrderBy:      mergeOrderBy,
		ShardLimit:        c.shardLimit,
		Aggregate:         c.aggregate,
		RedirectTarget:    redirTarget,
		RedirectAppend:    redirAppend,
	}, c, nil
}

// addSourcePredicates applies the source's filters: each pseudofile
// path segment maps to a column (namespace, env, service, kind), and a
// trace source to its trace or span columns. The path values are
// customer-supplied identifiers, so they go through positional
// placeholders rather than string interpolation — defense in depth
// against an adversarial namespace name even though the coordinator's
// CustomerHasSource allowlist gates the source up front.
func addSourcePredicates(sql *Query, src sourceInfo) {
	if src.env != "" {
		sql.addPredicate("env = ?", src.env)
	}
	addInPredicate(sql, "namespace", src.namespaces)
	addInPredicate(sql, "service", src.services)
	addInPredicate(sql, "kind", src.kinds)

	if src.cmd == "trace" && src.traceID != "" {
		sql.addPredicate("trace_id = ?", src.traceID)
	}
	if src.cmd == "trace_spans" {
		// Cross-trace aggregation (docs/plans/span-display.md, Phase
		// 2): no trace_id to anchor on — every "span done" (has a real
		// duration_ns) or "span error" (matched off raw, the only
		// signal that distinguishes it from a "span start" row, which
		// also has duration_ns=0 — see the plan's "Aggregation
		// mechanics") row in the queried time range, optionally
		// narrowed to one span name. The projection is still plain
		// `raw` (the default rawExpr) — no synthesized SQL expression
		// needed, since raw already holds weft-go's exact "span done
		// .../span error ..." text, which theme.RenderSpanRollup
		// already knows how to parse (both phases share one parser).
		sql.addPredicate("(duration_ns > 0 OR raw LIKE 'span error%')")
		if src.spanName != "" {
			sql.addPredicate("span_name = ?", src.spanName)
		}
	}
	if src.cmd == "trace" || src.cmd == "trace_spans" {
		// A trace (or a cross-trace span aggregation) fans out across
		// every service a request touched, with no shared context to
		// identify which service produced which line — unlike cat/tail
		// (single app, origin implicit from the path). Force the same
		// MetaVarColumns projection normally reserved for an explicit
		// awk/jq meta-var reference so NAMESPACE/SERVICE are always
		// real, pipeline-filterable fields, and so compute/engine.go's
		// trace-tag display branch has namespace/service available on
		// every row (SpanOccurrence.Label).
		sql.includeMetaVars = true
	}
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

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
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
