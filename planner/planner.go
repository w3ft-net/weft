package planner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// QueryPlan is the result of analyzing a weft pipeline. ShardSQL is the
// pushed-down SQL sent to shard nodes — always SELECT line_num, {raw_expr} as raw.
// ComputeStages lists the pipeline stages that must run in the compute layer.
type QueryPlan struct {
	ShardSQL       string
	ComputeStages  []string
	Limit          int           // post-merge limit (from head/tail), 0 = unlimited
	OrderDesc      bool          // true for tail (DESC), false for head (ASC)
	Follow         bool          // tail -f: keep stream open for live updates
	FollowApp      string        // app to subscribe to ("" = all)
	FollowPatterns []GrepPattern // grep filters applied to live rows
	// LiveStages is the post-source stage list to re-run per live row in
	// follow mode. Same strings as ComputeStages would have been if we
	// hadn't pushed anything down — i.e., a faithful reconstruction of
	// the user's pipeline minus the source. Consumed by
	// coordinator.BuildLiveFilter so the live broker rows go through
	// the same transformations as the historical batch.
	LiveStages []string
	Local          bool          // true if pipeline has no log source (run through warp locally)
	LocalPipeline  string        // the full pipeline string for local execution
	SourceCmd      string        // source command: "cat", "head", or "tail"
	SourceApp      string        // app extracted from pseudofile ("" = all apps)
	AlertArgs      []string      // argv for a terminal `alert` stage; nil if absent

	// Direct-to-compute mode (see docs/plans/direct-to-compute.md §3).
	// Populated from classifyShardMode walking ComputeStages for the
	// terminal sort. Consumed by compute's merge_key and by the
	// coordinator when taking the direct-to-compute path.
	ShardOrderBy string // "", "line_num[ DESC]", or "raw[ DESC]"
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

// Plan parses a weft shell pipeline and produces a QueryPlan by pushing
// down as many stages as possible into shard-level SQL. A trailing
// `| alert ...` stage is peeled off and returned as QueryPlan.AlertArgs
// for the coordinator (or cron) to evaluate against the query's output.
func Plan(pipeline string) (*QueryPlan, error) {
	queryPart, alertArgs := SplitAlertStage(pipeline)

	// Pipeline is just `alert ...` — nothing to query. The caller runs
	// AlertCommand with empty stdin (mostly useful for `alert absent`
	// or test-dispatch flows). A wholly empty input falls through so
	// parsePipeline reports the usual "empty pipeline" error.
	if queryPart == "" && alertArgs != nil {
		return &QueryPlan{Local: true, LocalPipeline: "", AlertArgs: alertArgs}, nil
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

	info, err := parsePipeline(queryPart)
	if err != nil {
		// If the source command is unknown, this is a local pipeline
		// (e.g., "echo 42 | alert above 0"). Same treatment for source
		// args that name a non-log shellfs namespace (/bin, /cron.d,
		// /etc, /tmp) — those reads go through warp+shellfs at
		// runtime, so any redirects honour FS write semantics.
		if strings.Contains(err.Error(), "unknown source command") || errors.Is(err, errLocalSourcePath) {
			// Preserve the original redirect alongside the local
			// pipeline so warp+shellfs can apply it natively at
			// exec time.
			local := queryPart
			if redirTarget != "" {
				op := ">"
				if redirAppend {
					op = ">>"
				}
				local = local + " " + op + " " + redirTarget
			}
			return &QueryPlan{Local: true, LocalPipeline: local, AlertArgs: alertArgs}, nil
		}
		return nil, fmt.Errorf("plan: %w", err)
	}

	sql := newSQLBuilder()

	// Snapshot the user's post-source stages before pushdown rewrites
	// them. LiveStages is a faithful reconstruction of "everything that
	// followed the source" — coordinator.BuildLiveFilter re-runs these
	// per row on the live-tail broker stream so the transformations
	// stay consistent with the historical batch.
	liveStages := formatComputeStages(info.stages)

	// Apply source: app filter + head/tail ordering.
	// One app: namespace OR-LIKE for bare names, exact match for sub-apps.
	// Many apps (multi-arg cat or post-glob expansion): single IN list.
	switch len(info.source.apps) {
	case 0:
		// warp.log — no app filter.
	case 1:
		app := info.source.apps[0]
		if strings.Contains(app, "/") {
			sql.addPredicate(fmt.Sprintf("app = '%s'", app))
		} else {
			sql.addPredicate(fmt.Sprintf("(app = '%s' OR app LIKE '%s/%%')", app, app))
		}
	default:
		quoted := make([]string, len(info.source.apps))
		for i, a := range info.source.apps {
			quoted[i] = "'" + a + "'"
		}
		sql.addPredicate(fmt.Sprintf("app IN (%s)", strings.Join(quoted, ", ")))
	}

	// Three-level pseudofile path /log/<ns>/<env>/<app>.log adds an
	// env predicate. See docs/plans/ingest-dimensions.md.
	if info.source.env != "" {
		sql.addPredicate(fmt.Sprintf("env = '%s'", info.source.env))
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

	sql.setSelect(rawExpr)

	orderDesc := sql.orderBy == "ts DESC"
	shardOrderBy, shardLimit := classifyShardMode(compute, orderDesc, sql.limit)

	return &QueryPlan{
		ShardSQL:       sql.build(),
		DirectShardSQL: sql.buildWith(shardOrderBy, shardLimit),
		ComputeStages:  compute,
		Limit:          sql.limit,
		OrderDesc:      orderDesc,
		Follow:         info.source.follow,
		FollowApp:      info.source.app(),
		FollowPatterns: grepPatterns,
		LiveStages:     liveStages,
		SourceCmd:      info.source.cmd,
		SourceApp:      info.source.app(),
		AlertArgs:      alertArgs,
		ShardOrderBy:   shardOrderBy,
		ShardLimit:     shardLimit,
		RedirectTarget: redirTarget,
		RedirectAppend: redirAppend,
	}, nil
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
func tryPushdown(stage stageInfo, sql *sqlBuilder) pushdownResult {
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
// it must round-trip as `''`, not collapse to nothing.
func needsShellQuote(s string) bool {
	if s == "" {
		return true
	}
	return strings.ContainsAny(s, shellMetachars)
}

// singleQuote wraps s in single quotes, escaping any embedded
// single-quote with the standard `'\''` close-reopen idiom.
func singleQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
