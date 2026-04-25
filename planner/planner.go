package planner

import (
	"fmt"
	"strings"
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

	info, err := parsePipeline(queryPart)
	if err != nil {
		// If the source command is unknown, this is a local pipeline
		// (e.g., "echo 42 | alert above 0"). Run it entirely through warp.
		if strings.Contains(err.Error(), "unknown source command") {
			return &QueryPlan{Local: true, LocalPipeline: queryPart, AlertArgs: alertArgs}, nil
		}
		return nil, fmt.Errorf("plan: %w", err)
	}

	sql := newSQLBuilder()

	// Apply source: app filter + head/tail ordering.
	// If the app contains "/" it's a specific sub-app (exact match).
	// Otherwise it's a namespace — match the namespace and all sub-apps.
	if info.source.app != "" {
		if strings.Contains(info.source.app, "/") {
			sql.addPredicate(fmt.Sprintf("app = '%s'", info.source.app))
		} else {
			sql.addPredicate(fmt.Sprintf("(app = '%s' OR app LIKE '%s/%%')", info.source.app, info.source.app))
		}
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
		FollowApp:      info.source.app,
		FollowPatterns: grepPatterns,
		SourceCmd:      info.source.cmd,
		SourceApp:      info.source.app,
		AlertArgs:      alertArgs,
		ShardOrderBy:   shardOrderBy,
		ShardLimit:     shardLimit,
	}, nil
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
		return pushdownResult{pushed: true, predicate: pd.predicate, compute: pd.compute}

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
		return pushdownResult{pushed: true, predicate: pred}

	case "sed":
		pred := trySedPushdown(stage.args)
		if pred == "" {
			return pushdownResult{}
		}
		return pushdownResult{pushed: true, predicate: pred}

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

func joinArgs(args []string) string {
	result := ""
	for i, a := range args {
		if i > 0 {
			result += " "
		}
		// Quote arguments that contain whitespace or are empty.
		if strings.ContainsAny(a, " \t") || a == "" {
			// If it's a flag+value combo like "-d ", split the flag prefix
			// from the value to produce: -d' '
			if len(a) > 2 && a[0] == '-' && !strings.HasPrefix(a, "--") {
				flag := a[:2]
				val := a[2:]
				result += flag + "'" + val + "'"
			} else {
				result += "'" + a + "'"
			}
		} else {
			result += a
		}
	}
	return result
}
