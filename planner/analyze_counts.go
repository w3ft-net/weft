package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// retrievalCount is the pipeline whose single output line is how many rows
// the optimized retrieval hands to the first stage that isn't optimized:
// the source and every stage before the barrier, then `wc -l`. It is empty
// when there is nothing worth counting: every stage is optimized (the
// pipeline is its own result), the source is a follow or isn't a log read,
// or the pipeline doesn't parse as a plain pipeline.
//
// The text is the user's own, each command unparsed from its AST as a stage's
// src is, so what it counts is what those stages produce when run. Stages
// before the barrier are optimized in whole, so the count is exact, never an
// upper bound from a prefilter.
func (c *compiled) retrievalCount(pipeline string, barrier int) string {
	if barrier == 0 || c.source.follow {
		return ""
	}
	switch c.source.cmd {
	case "cat", "head", "tail":
	default:
		return ""
	}
	script, err := bash.Parse(pipeline)
	if err != nil || len(script.Commands) != 1 || script.Commands[0].First == nil {
		return ""
	}
	cmds := script.Commands[0].First.Commands
	if barrier > len(cmds) {
		return ""
	}
	prefix := bash.Unparse(&bash.Script{Commands: []*bash.List{{First: &bash.Pipeline{Commands: cmds[:barrier]}}}})
	return strings.TrimSpace(prefix) + " | wc -l"
}

// Retrieved records the line count of the optimized retrieval, which the
// caller ran (Analysis.RetrievalCount) and parsed from the pipeline's single
// line of output.
func (a *Analysis) Retrieved(rows int64) {
	a.Rows, a.HasRows = rows, true
}

// rowsLine is the report line for the retrieval's line count, indented under
// the last optimized stage, or "" when there is nothing to say.
func (a *Analysis) rowsLine() string {
	lines := "lines"
	if a.Rows == 1 {
		lines = "line"
	}
	switch {
	case a.HasRows && a.Barrier == 1:
		return fmt.Sprintf("       %s %s retrieved\n", groupDigits(a.Rows), lines)
	case a.HasRows:
		return fmt.Sprintf("       %s %s retrieved with optimization\n", groupDigits(a.Rows), lines)
	case a.RowsNote != "":
		return fmt.Sprintf("       line count unavailable: %s\n", a.RowsNote)
	}
	return ""
}

// groupDigits is n with a comma between each group of three digits.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}
