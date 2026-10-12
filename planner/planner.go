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
	Limit         int // post-merge limit (from head/tail), 0 = unlimited
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
	LiveStages       []string
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

	// DirectShardSQL is the SQL the coordinator sends to shards via
	// QueryToCompute — same body as ShardSQL but with ShardOrderBy +
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
	compute   string // non-empty if the stage was partially pushed, leaving a compute remainder

	// limitSafe is set by stage-specific pushdown helpers when the
	// extracted predicate is safe to apply shard-side even after a
	// LIMIT has been frozen by an upstream head/tail. Today only awk
	// field/meta-var equality opts in; see
	// docs/plans/cross-shard-post-limit-filter.md for the conservative
	// default and the carve-out reasoning.
	limitSafe bool
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
		// real, pipeline-filterable fields, and so compute/local.go's
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
	var computeStrings []string    // formatted strings from partial pushdown
	var grepPatterns []GrepPattern // collected for follow mode
	pushing := true

	for _, stage := range info.stages {
		if !pushing {
			computeStages = append(computeStages, stage)
			continue
		}

		// Once a head/tail (or awk NR-window) bounds the source, every
		// later stage must operate on the *globally* merged top-N
		// stream. Pushing a filter shard-side after the LIMIT freezes
		// would let cross-shard merge inflate the post-filter set —
		// e.g. `tail -n 2000 | grep -v ERROR | wc -l` would saturate
		// at 2000 again because each shard returned its own ≤2000
		// non-ERROR rows. Compute already enforces the global cap
		// before stages run; route the post-limit stages there.
		// Pre-limit filters (filter | tail) are unaffected — the
		// freeze is set by the LIMIT pushdown itself, not by entering
		// the loop. See docs/plans/cross-shard-post-limit-filter.md.
		//
		// Carve-out: a stage that pushes a *limit-safe* predicate
		// (today: awk field/meta-var equality with a literal RHS)
		// gets through the gate. The user's intent on
		// `tail | awk '$N == "foo"'` is "recent rows where field N
		// matches" — pushing the predicate shard-side gives that
		// directly. Saturation hazards live in negation / inclusive
		// filters, not in value equality. The action remainder, when
		// present (filter + projection), is appended to compute
		// strings just like a non-frozen partial pushdown.
		if sql.limitFrozen {
			r := tryPushdown(stage, sql)
			if r.pushed && r.limitSafe {
				if r.predicate != "" {
					sql.addPredicate(r.predicate)
				}
				if r.compute != "" {
					computeStrings = append(computeStrings, r.compute)
				}
				continue
			}
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
			if r.compute != "" {
				pushing = false
				computeStrings = append(computeStrings, r.compute)
			}
		} else {
			pushing = false
			computeStages = append(computeStages, stage)
		}
	}

	// Build compute stage strings.
	var compute []string
	compute = append(compute, computeStrings...)
	for _, s := range computeStages {
		compute = append(compute, formatStage(s))
	}

	// Determine the raw expression for the SELECT clause. The projection
	// examines the first compute stage to see what columns/keys the pipeline
	// needs. extractRawExpr returns either:
	//   - residual=nil: the stage is fully covered, drop it
	//   - residual!=nil: a path prefix folded in; replace the stage with the
	//     residual and let it run in compute
	rawExpr := "raw"
	if len(computeStages) > 0 && len(computeStrings) == 0 {
		expr, residual, ok := extractRawExpr(computeStages[0])
		if ok {
			rawExpr = expr
			if residual == nil {
				compute = compute[:len(computeStrings)]
				compute = append(compute, formatComputeStages(computeStages[1:])...)
			} else {
				compute = compute[:len(computeStrings)]
				compute = append(compute, formatStage(*residual))
				compute = append(compute, formatComputeStages(computeStages[1:])...)
			}
		}
	}
	// Same treatment for the residual of a partial-pushdown stage (e.g. the
	// jq remainder after pushing select(...) — we may still be able to fold
	// a `.foo` projection out of it).
	if rawExpr == "raw" && len(computeStrings) > 0 {
		partial := parseComputeString(computeStrings[0])
		if partial != nil {
			expr, residual, ok := extractRawExpr(*partial)
			if ok {
				rawExpr = expr
				if residual == nil {
					compute = append(computeStrings[1:], formatComputeStages(computeStages)...)
				} else {
					compute = append([]string{formatStage(*residual)}, computeStrings[1:]...)
					compute = append(compute, formatComputeStages(computeStages)...)
				}
			}
		}
	}

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
	for _, s := range compute {
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
		ShardSQL:         shardSQL,
		Args:             shardArgs,
		ArgSources:       shardArgSources,
		DirectShardSQL:   directShardSQL,
		ComputeStages:    compute,
		Limit:            sql.limit,
		TailMode:         orderDesc,
		Follow:           info.source.follow,
		FollowPatterns:   grepPatterns,
		LiveStages:       liveStages,
		SourceCmd:        info.source.cmd,
		SourceCustomerID: customerID,
		SourceNamespace:  firstOrEmpty(info.source.namespaces),
		SourceService:    firstOrEmpty(info.source.services),
		SourceKind:       firstOrEmpty(info.source.kinds),
		AlertArgs:        alertArgs,
		ShardOrderBy:     mergeOrderBy,
		ShardLimit:       shardLimit,
		RedirectTarget:   redirTarget,
		RedirectAppend:   redirAppend,
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
		if len(stage.args) == 0 {
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
			compute:   pd.compute,
			limitSafe: pd.limitSafe,
		}

	case "jq":
		if len(stage.args) == 0 {
			return pushdownResult{}
		}
		flags, expr := splitJqArgs(stage.args)
		if expr == "" {
			return pushdownResult{}
		}
		return tryJqStagePushdown(expr, flags)

	case "grep":
		pred := tryGrepPushdown(stage.args)
		if pred == "" {
			return pushdownResult{}
		}
		// grep with -v is inverted/inclusive — not limit-safe (the
		// cross-shard saturation hazard the doc warned about). Plain
		// grep / grep -i is selective — limit-safe.
		_, _, invert := parseGrepArgs(stage.args)
		return pushdownResult{
			pushed:    true,
			predicate: pred,
			limitSafe: !invert,
		}

	case "sed":
		pred := trySedPushdown(stage.args)
		if pred == "" {
			return pushdownResult{}
		}
		// `sed -n '/pat/p'` is positive selection; limit-safe.
		return pushdownResult{
			pushed:    true,
			predicate: pred,
			limitSafe: true,
		}

	case "head":
		// A head/tail after an already-frozen LIMIT acts on the merged,
		// limited stream: leave the SQL alone and run it in compute.
		// Overwriting the LIMIT would turn `head -n 3 | tail -n 1` into
		// "the newest row" instead of "the third".
		if sql.limitFrozen {
			return pushdownResult{}
		}
		n := parseLineCount(stage.args)
		sql.setOrderLimit("ts ASC", n)
		return pushdownResult{pushed: true}

	case "tail":
		if sql.limitFrozen {
			return pushdownResult{}
		}
		n := parseLineCount(stage.args)
		sql.setOrderLimit("ts DESC", n)
		return pushdownResult{pushed: true}
	}

	return pushdownResult{}
}

// formatStage converts a stageInfo back to a string representation for compute.
func formatStage(s stageInfo) string {
	if len(s.args) == 0 {
		return s.cmd
	}
	// Commands whose first non-flag argument is a program need quoting.
	switch s.cmd {
	case "awk", "jq", "sed":
		return formatToolStage(s.cmd, s.args)
	}
	return s.cmd + " " + joinArgs(s.args)
}

// formatToolStage quotes the program argument for awk/jq/sed.
func formatToolStage(cmd string, args []string) string {
	var parts []string
	quoted := false
	for _, a := range args {
		if !quoted && !strings.HasPrefix(a, "-") {
			parts = append(parts, "'"+a+"'")
			quoted = true
		} else {
			parts = append(parts, a)
		}
	}
	return cmd + " " + strings.Join(parts, " ")
}

func formatComputeStages(stages []stageInfo) []string {
	var out []string
	for _, s := range stages {
		out = append(out, formatStage(s))
	}
	return out
}

// parseComputeString tries to parse a formatted compute string like "awk '{ print $3 }'"
// or "jq -r 'select(.x==1)'" back into a stageInfo. Used for extracting
// projections from partial pushdown remainders. Handles leading flags before
// the single-quoted expression.
func parseComputeString(s string) *stageInfo {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	cmd := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(s, cmd))
	if rest == "" {
		return &stageInfo{cmd: cmd}
	}
	// Split on the first single-quote: everything before is flags, the
	// quoted portion is the expression arg. If there's no quote, fall
	// back to whitespace-split.
	quoteIdx := strings.IndexByte(rest, '\'')
	if quoteIdx < 0 {
		return &stageInfo{cmd: cmd, args: fields[1:]}
	}
	var args []string
	if flagPart := strings.TrimSpace(rest[:quoteIdx]); flagPart != "" {
		args = append(args, strings.Fields(flagPart)...)
	}
	quoted := rest[quoteIdx:]
	if len(quoted) >= 2 && quoted[len(quoted)-1] == '\'' {
		args = append(args, quoted[1:len(quoted)-1])
	} else {
		args = append(args, quoted)
	}
	return &stageInfo{cmd: cmd, args: args}
}

// shellMetachars must be quoted so the joined compute-stage script
// re-parses to the same argv when the compute pool feeds it back to
// the bash interpreter. Whitespace alone wasn't enough — `grep
// '(empty)'` joined as bare `grep (empty)` parses as a subshell and
// grep ends up with no positional arg ("missing pattern" in prod).
const shellMetachars = " \t\n();&|<>*?[]{}~!#$`'\"\\"

func joinArgs(args []string) string {
	result := ""
	for i, a := range args {
		if i > 0 {
			result += " "
		}
		if needsShellQuote(a) {
			// If it's a short flag bundling its value (`-d `), keep the
			// flag prefix outside the quotes so the quoted form is
			// `-d' '` rather than `'-d '` — same shape awk/sed expect.
			if len(a) > 2 && a[0] == '-' && !strings.HasPrefix(a, "--") {
				flag := a[:2]
				val := a[2:]
				result += flag + singleQuote(val)
			} else {
				result += singleQuote(a)
			}
		} else {
			result += a
		}
	}
	return result
}

// needsShellQuote reports whether s would be reinterpreted by the
// bash parser if emitted bare. Empty string requires quoting too —
// it must round-trip as `”`, not collapse to nothing.
func needsShellQuote(s string) bool {
	if s == "" {
		return true
	}
	return strings.ContainsAny(s, shellMetachars)
}

// singleQuote wraps s in single quotes, escaping any embedded
// single-quote with the standard `'\”` close-reopen idiom.
func singleQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
