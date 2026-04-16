package planner

import (
	"fmt"
	"strings"
)

// Explain runs the planner on a pipeline and returns a human-readable
// breakdown of the query plan: source, shard SQL, and compute stages.
func Explain(pipeline string) string {
	plan, err := Plan(pipeline)
	if err != nil {
		return fmt.Sprintf("Error: %v\n", err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Pipeline: %s\n", pipeline)

	if plan.Local {
		fmt.Fprintf(&b, "\nLocal pipeline (no log source):\n  %s\n", plan.LocalPipeline)
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
