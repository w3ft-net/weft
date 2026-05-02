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
func Explain(pipeline string) string {
	plan, err := Plan(pipeline, "", time.Time{}, time.Time{})
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
	app := plan.SourceApp
	if app == "" {
		app = "(all apps)"
	}
	fmt.Fprintf(&b, "  app:      %s\n", app)
	fmt.Fprintf(&b, "  limit:    %d\n", plan.Limit)
	fmt.Fprintf(&b, "  follow:   %v\n", plan.Follow)

	// Shard SQL.
	fmt.Fprintf(&b, "\nShard SQL:\n")
	for _, line := range formatSQL(plan.ShardSQL) {
		fmt.Fprintf(&b, "  %s\n", line)
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
// for readability.
func formatSQL(sql string) []string {
	keywords := []string{" WHERE ", " AND ", " ORDER BY ", " LIMIT ", " OFFSET "}
	result := sql
	for _, kw := range keywords {
		result = strings.ReplaceAll(result, kw, "\n"+strings.TrimLeft(kw, " "))
	}
	return strings.Split(result, "\n")
}
