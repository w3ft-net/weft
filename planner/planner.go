package planner

import (
	"fmt"
	"strings"
)

// QueryPlan is the result of analyzing a weft pipeline. ShardSQL is the
// pushed-down SQL sent to shard nodes. ComputeStages lists the pipeline
// stages that must run in the gpipe compute layer, in order.
// GrepPattern captures a grep filter for live tail matching.
type GrepPattern struct {
	Pattern    string
	IgnoreCase bool
	Invert     bool
}

type QueryPlan struct {
	ShardSQL       string
	ComputeStages  []string
	Limit          int           // post-merge limit (from head/tail), 0 = unlimited
	OrderDesc      bool          // true for tail (DESC), false for head (ASC)
	Project        *Project      // field/key projection extracted from cut/awk/jq, or nil
	Follow         bool          // tail -f: keep stream open for live updates
	FollowApp      string        // app to subscribe to ("" = all)
	FollowPatterns []GrepPattern // grep filters applied to live rows
}

// Project describes which fields or JSON keys a pipeline needs from the shard.
// The shard can narrow its SELECT to only these columns, reducing data transfer.
type Project struct {
	Fields   []int    // positional field indices (1-based); negative = open-ended (e.g., -5 means "5 to end")
	JSONKeys []string // JSON payload keys needed
	RawExpr  string   // SQL expression for raw column; overrides Fields-based reconstruction when set
	Complete bool     // if true, the projection fully replaces the stage (no compute needed)
}

// pushdownResult is the outcome of attempting to push a stage into SQL.
type pushdownResult struct {
	pushed    bool   // true if at least a predicate was extracted
	predicate string // SQL WHERE fragment (may be "" even if pushed, e.g. head/tail)
	compute   string // non-empty if the stage was partially pushed, leaving a compute remainder
}

// Plan parses a weft shell pipeline and produces a QueryPlan by pushing
// down as many stages as possible into shard-level SQL.
func Plan(pipeline string) (*QueryPlan, error) {
	info, err := parsePipeline(pipeline)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	sql := newSQLBuilder()

	// Apply source: app filter + head/tail ordering.
	if info.source.app != "" {
		sql.addPredicate(fmt.Sprintf("app = '%s'", info.source.app))
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
	var computeStrings []string     // formatted strings from partial pushdown
	var grepPatterns []GrepPattern  // collected for follow mode
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

	// Extract projection from the first compute stage (raw, not partial).
	// Only safe when there are no partial pushdown remainders (computeStrings)
	// running before it — those may transform the data, making the shard's
	// fields column stale for projection.
	var proj *Project
	if len(computeStages) > 0 && len(computeStrings) == 0 {
		proj = extractProjection(computeStages[0])
		if proj != nil && proj.Complete {
			// Complete projection replaces the first compute stage.
			compute = compute[:len(computeStrings)]
			compute = append(compute, formatComputeStages(computeStages[1:])...)
			if len(compute) > 0 {
				proj.Complete = false // still have downstream compute
			}
		}
	}
	// Also check partial pushdown compute strings for awk projections.
	if proj == nil && len(computeStrings) > 0 {
		// Parse the partial pushdown string to extract projection.
		partial := parseComputeString(computeStrings[0])
		if partial != nil {
			proj = extractProjection(*partial)
			if proj != nil && proj.Complete {
				// Complete projection replaces the partial compute string.
				compute = append(computeStrings[1:], formatComputeStages(computeStages)...)
				if len(compute) > 0 {
					proj.Complete = false // still have downstream compute
				}
			}
		}
	}

	return &QueryPlan{
		ShardSQL:       sql.build(),
		ComputeStages:  compute,
		Limit:          sql.limit,
		OrderDesc:      sql.orderBy == "ts DESC",
		Project:        proj,
		Follow:         info.source.follow,
		FollowApp:      info.source.app,
		FollowPatterns: grepPatterns,
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
		return tryJqStagePushdown(stage.args[0])

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
// back into a stageInfo. Used for extracting projections from partial pushdown remainders.
func parseComputeString(s string) *stageInfo {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	cmd := fields[0]
	var args []string
	// Rejoin the rest, handling quoted arguments.
	rest := strings.TrimSpace(strings.TrimPrefix(s, cmd))
	if rest != "" {
		// Strip surrounding single quotes if present.
		if len(rest) >= 2 && rest[0] == '\'' && rest[len(rest)-1] == '\'' {
			args = []string{rest[1 : len(rest)-1]}
		} else {
			args = fields[1:]
		}
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
