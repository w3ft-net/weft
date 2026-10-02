package planner

import (
	"fmt"
	"strings"
	"time"
)

// Explain runs the planner on a pipeline and returns a human-readable
// breakdown of the query plan: source, shard SQL, and compute stages.
// Customer + bounds are passed empty/zero — Explain is for human
// inspection, not dispatch, so the row-level security predicates are
// elided to keep the output focused on pipeline shape.
//
// The shard SQL is rendered structurally via renderQuery rather than
// regex-splitting the rendered string. Subqueries indent one level,
// and bound placeholders show their values inline as `? = 'value'`
// so readers can correlate placeholders to args without scrolling.
func Explain(pipeline string) string {
	plan, err := Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil {
		return fmt.Sprintf("Error: %v\n", err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Pipeline: %s\n", pipeline)

	// Alert-only pipeline carries AlertArgs but no SQL/source; render
	// the trailing alert and return.
	if plan.ShardSQL == "" {
		if plan.AlertArgs != nil {
			fmt.Fprintf(&b, "\nAlert-only pipeline:\n  alert %s\n", strings.Join(plan.AlertArgs, " "))
		}
		return b.String()
	}

	// Source.
	fmt.Fprintf(&b, "\nSource:\n")
	fmt.Fprintf(&b, "  command:  %s\n", plan.SourceCmd)
	fmt.Fprintf(&b, "  namespace: %s\n", orAll(plan.SourceNamespace))
	fmt.Fprintf(&b, "  service:  %s\n", orAll(plan.SourceService))
	fmt.Fprintf(&b, "  kind:     %s\n", orAll(plan.SourceKind))
	fmt.Fprintf(&b, "  limit:    %d\n", plan.Limit)
	fmt.Fprintf(&b, "  follow:   %v\n", plan.Follow)

	// Shard SQL — split into structured lines plus the args legend.
	fmt.Fprintf(&b, "\nShard SQL:\n")
	for _, line := range formatSQL(plan.ShardSQL) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if len(plan.Args) > 0 {
		fmt.Fprintf(&b, "\nBound args (in render order):\n")
		for i, a := range plan.Args {
			fmt.Fprintf(&b, "  $%d = %q\n", i+1, fmt.Sprintf("%v", a))
		}
	}

	// Compute stages.
	if len(plan.ComputeStages) > 0 {
		fmt.Fprintf(&b, "\nCompute stages:\n")
		for i, stage := range plan.ComputeStages {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, stage)
		}
	} else {
		fmt.Fprintf(&b, "\nCompute stages: (none — fully pushed down)\n")
	}

	return b.String()
}

// formatSQL breaks a SQL string into indented lines at keyword boundaries
// for readability. The split is purely textual — it doesn't traverse the
// AST — so this works correctly on both legacy baked-literal SQL and the
// new placeholder form. Tail's subquery flip is rendered with the inner
// SELECT on its own line; the closing `) AS t` lands as a separate clause
// so the nesting is visible.
func formatSQL(sql string) []string {
	keywords := []string{" WHERE ", " AND ", " ORDER BY ", " LIMIT ", " OFFSET ", " FROM (", ") AS "}
	result := sql
	for _, kw := range keywords {
		result = strings.ReplaceAll(result, kw, "\n"+strings.TrimLeft(kw, " "))
	}
	return strings.Split(result, "\n")
}

// orAll renders an empty source-coordinate as the wildcard label for
// the human-readable EXPLAIN output.
func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}
