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
	// Populated from classifyShardMode walking ComputeStages for the
	// terminal sort. Consumed by compute's merge_key and by the
	// coordinator when taking the direct-to-compute path.
	ShardOrderBy string // "", "ts[ DESC]", or "raw[ DESC]"
	ShardLimit   int    // per-shard LIMIT; 0 when pushdown is unsafe

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

// GrepPattern captures a grep filter for live tail matching.
type GrepPattern struct {
	Pattern    string
	IgnoreCase bool
	Invert     bool
}

// pushdownResult is the outcome of attempting to push a stage into SQL.
// predicate is a raw SQL fragment with literals baked in — the per-
// stage emitters (grep, awk, sed, jq, project) still produce string
// predicates and rely on escapeSQLString for safety. Step 8b (a
// follow-on) migrates them to Predicate{SQL, Args}. The pre-stage
// adders in Plan() (customer/timestamps/namespace/env/service) DO use
// placeholders today — they're the highest-value parameterization
// targets (customer is the row-level security boundary; ts shifts
// every request and is the biggest plan-cache miss).
type pushdownResult struct {
	pushed    bool   // true if at least a predicate was extracted
	predicate string // SQL WHERE fragment (may be "" even if pushed, e.g. head/tail)
	// keep marks predicate as a prefilter: the stage still runs,
	// verbatim, in compute. Without it the predicate is exact and the
	// stage is dropped.
	keep bool
}

// ErrNotPlannable is the typed sentinel Plan returns when a pipeline
// has no /log/... source — i.e. it isn't something the planner can
// turn into shard SQL. The coordinator's rewriter classifies log
// pipelines lexically (see query_rewrite.go), so this error mostly
// surfaces from the rewriter callback re-planning a substituted
// pipeline whose shape unexpectedly changed.
var ErrNotPlannable = errors.New("not a plannable log query")

func Plan(pipeline string, customerID int64, cwd string, start, end time.Time) (*QueryPlan, error) {
	queryPart, alertArgs := SplitAlertStage(pipeline)

	// Alert-only pipeline (`alert ...` with nothing in front). Return
	// a plan that carries only the alert args; the empty ShardSQL is
	// the caller's signal to short-circuit shard execution.
	if queryPart == "" && alertArgs != nil {
		return &QueryPlan{AlertArgs: alertArgs}, nil
	}

	// Strip a trailing `> /tmp/...` (or other shellfs namespace)
	// redirect off the query pipeline before classification. Without
	// this the redirect would be silently dropped by executeQuery —
	// the result rows never reach FS write machinery. We capture the
	// target and rebuild queryPart from the surviving AST.
	queryPart, redirTarget, redirAppend, err := extractTrailingFSRedirect(queryPart)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	info, err := parsePipeline(queryPart, cwd)
	if err != nil {
		// Not a log query — non-/log/ source path or an unknown
		// source command. Caller (Coordinator.Execute) routes these
		// to warp via runLocal; ErrNotPlannable is the typed signal.
		if strings.Contains(err.Error(), "unknown source command") || errors.Is(err, errLocalSourcePath) {
			return nil, ErrNotPlannable
		}
		return nil, fmt.Errorf("plan: %w", err)
	}

	if info.source.follow && redirTarget != "" {
		return nil, fmt.Errorf("plan: %w", FollowRedirectError(redirTarget))
	}

	sql := newSQLBuilder()
	// Customer + time bounds first so they sit at the head of the
	// WHERE clause regardless of subsequent app/source/stage pushdowns.
	// addCustomer/addTimeBounds are no-ops on empty/zero values.
	sql.addCustomer(customerID)
	sql.addTimeBounds(start, end)

	// Snapshot the user's post-source stages before pushdown rewrites
	// them. LiveStages is a faithful reconstruction of "everything that
	// followed the source" — coordinator.BuildLiveFilter re-runs these
	// per row on the live-tail broker stream so the transformations
	// stay consistent with the historical batch.
	liveStages := formatComputeStages(info.stages)
	// Live rows follow the same rule, one row at a time: leading row
	// stages keep or drop the row, and a jq after them reads its
	// payload. There's no SQL projection to fold into on the live path.
	liveRows, liveRest := splitRowStages(info.stages)
	liveRowStages := formatComputeStages(liveRows)
	liveReadsPayload := len(liveRest) > 0 && liveRest[0].cmd == "jq"
	if len(liveRows) > 0 {
		liveStages = formatComputeStages(liveRest)
	}

	// Apply source filters from the pseudofile path. Each path segment
	// maps to a column: namespace, env, service, kind. These are
	// customer-supplied identifiers, so they go through positional
	// placeholders rather than string interpolation — defense in depth
	// against an adversarial namespace name even though the
	// coordinator's CustomerHasSource allowlist gates the source up
	// front.
	if info.source.env != "" {
		sql.addPredicate("env = ?", info.source.env)
	}
	switch len(info.source.namespaces) {
	case 0:
		// warp.log — no namespace/service/kind filter.
	case 1:
		sql.addPredicate("namespace = ?", info.source.namespaces[0])
	default:
		placeholders := strings.Repeat("?, ", len(info.source.namespaces)-1) + "?"
		sql.addPredicate("namespace IN ("+placeholders+")", info.source.namespaces...)
	}
	switch len(info.source.services) {
	case 0:
		// No service filter (namespace-level wildcard or warp.log).
	case 1:
		sql.addPredicate("service = ?", info.source.services[0])
	default:
		placeholders := strings.Repeat("?, ", len(info.source.services)-1) + "?"
		sql.addPredicate("service IN ("+placeholders+")", info.source.services...)
	}
	switch len(info.source.kinds) {
	case 0:
		// No kind filter (namespace wildcard or warp.log).
	case 1:
		sql.addPredicate("kind = ?", info.source.kinds[0])
	default:
		placeholders := strings.Repeat("?, ", len(info.source.kinds)-1) + "?"
		sql.addPredicate("kind IN ("+placeholders+")", info.source.kinds...)
	}

	if info.source.cmd == "trace" && info.source.traceID != "" {
		sql.addPredicate("trace_id = ?", info.source.traceID)
	}
	if info.source.cmd == "trace_spans" {
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
		if info.source.spanName != "" {
			sql.addPredicate("span_name = ?", info.source.spanName)
		}
	}
	if info.source.cmd == "trace" || info.source.cmd == "trace_spans" {
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

	switch info.source.cmd {
	case "head":
		sql.setOrderLimit("ts ASC", info.source.limit)
	case "tail":
		sql.setOrderLimit("ts DESC", info.source.limit)
	}

	// Walk stages left-to-right, pushing down as many as possible.
	// Keep raw stageInfo for compute stages so we can extract projections.
	var computeStages []stageInfo
	var grepPatterns []GrepPattern // collected for follow mode
	pushing := true

	for _, stage := range info.stages {
		if !pushing {
			computeStages = append(computeStages, stage)
			continue
		}

		// Once a head/tail line count (or awk NR-window) bounds the
		// source, every later stage runs in compute on the globally
		// merged stream — no later predicate is pushed into shard SQL,
		// selective or not. `tail -n N | filter` means "the last N rows,
		// then filter", so `tail -n 500 | awk 'LEVEL == "ERROR"' | wc -l`
		// counts errors among 500 rows instead of returning the last 500
		// errors (which can never fall below 500 and breaks alert
		// recipes). A head/tail stage after the freeze lands here too,
		// so it can't overwrite the LIMIT. Pre-limit filters
		// (filter | tail) are unaffected: the freeze is set by the LIMIT
		// pushdown itself, not by entering the loop. See
		// docs/completed/cross-shard-post-limit-filter.md.
		if sql.limitFrozen {
			pushing = false
			computeStages = append(computeStages, stage)
			continue
		}

		r := tryPushdown(stage, sql)
		if r.pushed {
			if r.predicate != "" {
				sql.addPredicate(r.predicate)
			}
			// Collect grep patterns for follow mode (only if successfully pushed down).
			if stage.cmd == "grep" {
				if pattern, ignoreCase, invert := parseGrepArgs(stage.args); pattern != "" {
					grepPatterns = append(grepPatterns, GrepPattern{
						Pattern:    pattern,
						IgnoreCase: ignoreCase,
						Invert:     invert,
					})
				}
			}
			if r.keep {
				pushing = false
				stage.prefiltered = true
				computeStages = append(computeStages, stage)
			}
		} else {
			pushing = false
			computeStages = append(computeStages, stage)
		}
	}

	// Compute's first stage reads shard rows rather than text, and so
	// does every stage after row stages that pass rows through. When
	// row stages are followed by a stage that reads row metadata, they
	// run per row in the engine (RowStages) and the rows reach the next
	// stage whole; a jq there reads the payload (StagesReadPayload).
	// Otherwise the first stage may be exactly a projection SQL computes
	// (awk '{ print LEVEL }', jq '.a'), which becomes the SELECT, or a
	// jq reading the payload, which SQL selects in raw's place.
	rowStages, computeStages := splitRowStages(computeStages)
	rawExpr := "raw"
	var stagesReadPayload bool
	if len(rowStages) > 0 {
		stagesReadPayload = len(computeStages) > 0 && computeStages[0].cmd == "jq"
		sql.includePayload = stagesReadPayload
		for _, rs := range rowStages {
			sql.includePayload = sql.includePayload || rs.cmd == "jq"
		}
	} else if len(computeStages) > 0 {
		first := computeStages[0]
		if first.cmd == "jq" {
			if expr := lowerFirstJqStage(first, first.prefiltered); expr != "" {
				rawExpr = expr
				computeStages = computeStages[1:]
			} else {
				rawExpr = "CAST(payload AS VARCHAR)"
			}
		} else if expr, ok := extractRawExpr(first); ok {
			rawExpr = expr
			computeStages = computeStages[1:]
		}
	}
	compute := formatComputeStages(computeStages)

	// A remaining jq stage might read a bare "._"-prefixed meta-variable
	// for output (e.g. `jq -r '._host'`) rather than only inside a
	// pushed-down select() predicate — the case jqComparisonToSQL/
	// jqValueToSQL already handle. Project every MetaVarColumns column
	// alongside raw so compute (compute/metavars.go) can splice the
	// values into each row's JSON body before the jq stage runs.
	//
	// Textual substring check on the jq stage's own source, not an AST
	// walk: a meta-variable reference can only ever appear in a jq
	// program as the literal token "._name" (there's no aliasing or
	// indirection in this expression language), so this has no false
	// negatives — and a false positive (e.g. the sequence happening to
	// appear inside a string literal) only costs a few extra, often-NULL
	// columns per row, not a correctness bug. Deliberately not a full
	// AST-recognizer layer for what's still a bounded-size feature.
	// A remaining awk stage might read a bare reserved meta-variable
	// (e.g. `HOST`) for output the same way — see docs/known-issues.md's
	// now-fixed awk entry. Unlike jq's "._name" token, a bare
	// identifier has no distinguishing prefix, so this uses a
	// word-bounded regex (awkMetaVarPattern) rather than a plain
	// substring check, to avoid matching inside an unrelated longer
	// identifier (e.g. "MYHOST"). Same bounded-feature tradeoff as the
	// jq check: a false positive (the reserved name happening to be the
	// user's own unrelated awk variable) only costs a few extra columns.
	metaReaders := append(formatComputeStages(rowStages), compute...)
	for _, s := range metaReaders {
		if strings.HasPrefix(s, "jq") {
			for metaVar := range JQMetaVars {
				if strings.Contains(s, "."+metaVar) {
					sql.includeMetaVars = true
					break
				}
			}
		} else if strings.HasPrefix(s, "awk") && awkMetaVarPattern.MatchString(s) {
			sql.includeMetaVars = true
		}
		if sql.includeMetaVars {
			break
		}
	}

	sql.setSelect(rawExpr)

	orderDesc := sql.orderBy == "ts DESC"
	shardOrderBy, shardLimit := classifyShardMode(compute, orderDesc, sql.limit)

	// tail wraps its inner DESC + LIMIT pushdown in an outer ASC re-emit
	// so the user-visible rows arrive in chronological order. The inner
	// keeps doing the cheap per-shard top-N work; the outer flip is a
	// bounded re-sort. ShardSQL preserves the head/tail-derived inner
	// ordering verbatim, so we wrap iff the inner is "ts DESC". The
	// DirectShardSQL path's inner uses the mode-classified order from
	// classifyShardMode — wrap whenever that's "ts DESC" (Mode A with
	// tail). Other DESC keys (raw DESC from sort -r) are explicit user
	// intent and not wrapped.
	shardQuery := sql
	if sql.orderBy == "ts DESC" {
		shardQuery = sql.Wrap("ts ASC")
	}
	shardSQL, shardArgs, shardArgSources := shardQuery.ToSQLWithSources()

	directInner := sql.WithOrderLimit(shardOrderBy, shardLimit)
	directQuery := directInner
	if shardOrderBy == "ts DESC" {
		directQuery = directInner.Wrap("ts ASC")
	}
	directShardSQL, _ := directQuery.ToSQL()
	// ShardSQL and DirectShardSQL render identical WHERE clauses (only
	// order/limit differ), so their args slices match — Args lives once
	// on the plan rather than duplicating per build path.

	// After the wrap, rows reach compute in ts ASC even though the
	// inner pushdown was DESC. Reflect that in the merge-key the
	// coordinator forwards to compute, otherwise the k-way merge
	// walks the per-shard streams in the wrong direction.
	mergeOrderBy := shardOrderBy
	if shardOrderBy == "ts DESC" {
		mergeOrderBy = "ts"
	}

	return &QueryPlan{
		ShardSQL:          shardSQL,
		Args:              shardArgs,
		ArgSources:        shardArgSources,
		DirectShardSQL:    directShardSQL,
		ComputeStages:     compute,
		RowStages:         formatComputeStages(rowStages),
		StagesReadPayload: stagesReadPayload,
		Limit:             sql.limit,
		TailMode:          orderDesc,
		Follow:            info.source.follow,
		FollowPatterns:    grepPatterns,
		LiveStages:        liveStages,
		LiveRowStages:     liveRowStages,
		LiveReadsPayload:  liveReadsPayload,
		SourceCmd:         info.source.cmd,
		SourceCustomerID:  customerID,
		SourceNamespace:   firstOrEmpty(info.source.namespaces),
		SourceService:     firstOrEmpty(info.source.services),
		SourceKind:        firstOrEmpty(info.source.kinds),
		AlertArgs:         alertArgs,
		ShardOrderBy:      mergeOrderBy,
		ShardLimit:        shardLimit,
		RedirectTarget:    redirTarget,
		RedirectAppend:    redirAppend,
	}, nil
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

// tryPushdown attempts to push a single stage into the SQL builder.
func tryPushdown(stage stageInfo, sql *Query) pushdownResult {
	switch stage.cmd {
	case "awk":
		// Flags (-F, -v) change how awk splits and what its variables
		// hold, so only a bare program is analyzed.
		if len(stage.args) != 1 || strings.HasPrefix(stage.args[0], "-") {
			return pushdownResult{}
		}
		pd := tryAwkPushdown(stage.args[0])
		if pd == nil {
			return pushdownResult{}
		}
		if pd.limit > 0 || pd.offset > 0 {
			sql.setLimitOffset(pd.limit, pd.offset)
			return pushdownResult{pushed: true}
		}
		return pushdownResult{
			pushed:    true,
			predicate: pd.predicate,
			keep:      !pd.exact,
		}

	case "jq":
		if len(stage.args) == 0 {
			return pushdownResult{}
		}
		return tryJqStagePushdown(stage.args)

	case "grep":
		pred := tryGrepPushdown(stage.args)
		if pred == "" {
			return pushdownResult{}
		}
		return pushdownResult{
			pushed:    true,
			predicate: pred,
		}

	case "sed":
		pred := trySedPushdown(stage.args)
		if pred == "" {
			return pushdownResult{}
		}
		return pushdownResult{
			pushed:    true,
			predicate: pred,
		}

	case "head":
		n := parseLineCount(stage.args)
		sql.setOrderLimit("ts ASC", n)
		return pushdownResult{pushed: true}

	case "tail":
		n := parseLineCount(stage.args)
		sql.setOrderLimit("ts DESC", n)
		return pushdownResult{pushed: true}
	}

	return pushdownResult{}
}

// formatComputeStages returns each stage's own source text.
func formatComputeStages(stages []stageInfo) []string {
	var out []string
	for _, s := range stages {
		out = append(out, s.src)
	}
	return out
}
