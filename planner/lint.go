package planner

import (
	"fmt"
	"strings"
)

// LintTruncationRisk inspects a save-time LoweredPlan for a pipeline
// shape that can silently drop data within a single run: a head/tail
// row cap on the source (Limit > 0) combined with a downstream stage
// that summarizes across the *whole* window (a count, a distinct-
// count, a length/reduce) rather than merely selecting the most
// recent few rows.
//
// A frozen LIMIT alone isn't sufficient to warn on — plenty of crons
// cap intentionally as a "give me the last N samples" query, where
// truncation is the point, not a bug (e.g. `tail -n 5 ... | jq -r
// .fds_open | sort -rn | head -1`). The distinguishing signal is
// whether some stage aggregates across every row in the capped
// window; see aggregatingStage.
//
// Purely static and pattern-based by design — no query against real
// log volume, so a generous-looking N that's still too small for
// actual traffic isn't caught. Ambiguous shapes are deliberately
// biased toward not warning: a missed risk costs nothing new, a
// noisy false positive on a legitimate sampling cron teaches people
// to ignore the warning.
func LintTruncationRisk(lp *LoweredPlan) []string {
	if lp == nil || lp.Limit <= 0 {
		return nil
	}
	stage := aggregatingStage(lp.ComputeStages)
	if stage == "" {
		return nil
	}
	source := lp.SourceCmd
	if source == "" {
		source = "tail"
	}
	return []string{fmt.Sprintf(
		"`%s -n %d` caps this cron at %d matching rows per run — if the source produces more than that in one run, the excess is dropped before `%s` sees it, and the result will be silently low. Consider raising -n or narrowing the match earlier in the pipeline.",
		source, lp.Limit, lp.Limit, stage,
	)}
}

// aggregatingStage returns the first compute stage that summarizes
// across every row in the window — `wc -l`, `uniq -c`, `grep -c`, or
// a `jq` length/reduce expression — or "" if none is found. A
// recency-select tail (`sort | head -1`, bare `head -N`) with no such
// stage returns "": the cap there is the intended behavior.
func aggregatingStage(stages []string) string {
	for _, s := range stages {
		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "wc":
			return s
		case "uniq":
			for _, f := range fields[1:] {
				if f == "-c" {
					return s
				}
			}
		case "grep":
			for _, f := range fields[1:] {
				if f == "-c" {
					return s
				}
			}
		case "jq":
			rest := strings.Join(fields[1:], " ")
			if strings.Contains(rest, "length") || strings.Contains(rest, "reduce ") {
				return s
			}
		}
	}
	return ""
}
