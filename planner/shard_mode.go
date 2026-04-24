package planner

import "strings"

// classifyShardMode decides the shard's ORDER BY + LIMIT for the
// direct-to-compute path. See docs/plans/direct-to-compute.md §3:
//
//	Mode A (default — no sort) → "line_num" ordering, direction from
//	                             a terminal head/tail, limit = plan.Limit.
//	Mode B (plain sort first)  → "raw" ordering, direction from sort -r.
//	                             Limit only if already pushed (plan.Limit).
//	Mode C (sort flags, or any transforming stage before sort) → no
//	                             shard-side order / limit; compute
//	                             handles everything.
//
// computeStages is the final, formatted list from the planner (the
// same strings that become QueryPlan.ComputeStages). order is the
// shard-facing merge_key (empty in Mode C); limit mirrors plan.Limit
// in Modes A/B and is 0 in Mode C.
//
// The "Mode B only when `sort` is the first compute stage" rule is
// the plan's safe-first-cut: any transforming stage (awk/jq/sed)
// between the shard and the sort rewrites row bytes, so the shard's
// ORDER BY raw no longer matches what compute's sort compares.
// Tightening this later (e.g. allowing pure-filter stages before
// sort) is straightforward — widen the acceptable-pre-sort set here.
func classifyShardMode(computeStages []string, orderDesc bool, limit int) (order string, shardLimit int) {
	sortIdx, sortFields := findLastSort(computeStages)

	// --- Mode A: no sort anywhere ---------------------------------
	if sortIdx < 0 {
		if orderDesc {
			return "line_num DESC", limit
		}
		return "line_num", limit
	}

	// --- Inspect sort flags ---------------------------------------
	hasTransformingFlag := false
	reverse := false
	for _, f := range sortFields[1:] {
		switch f {
		case "-r":
			reverse = true
		default:
			if strings.HasPrefix(f, "-") {
				// -n, -k, -f, -h, etc. — all change the compare key
				// away from raw bytes, so shard-side ORDER BY raw
				// can't produce the global order compute needs.
				hasTransformingFlag = true
			}
		}
	}
	if hasTransformingFlag {
		return "", 0
	}

	// --- Mode B eligibility: sort must be the first compute stage ---
	if sortIdx != 0 {
		return "", 0
	}

	// Plain `sort` / `sort -r`.
	if reverse {
		return "raw DESC", limit
	}
	return "raw", limit
}

// findLastSort returns the index of the last compute stage whose
// first field is "sort" plus its tokenized fields, or -1/nil when no
// such stage exists.
func findLastSort(stages []string) (int, []string) {
	lastIdx := -1
	var lastFields []string
	for i, s := range stages {
		fields := strings.Fields(s)
		if len(fields) > 0 && fields[0] == "sort" {
			lastIdx = i
			lastFields = fields
		}
	}
	return lastIdx, lastFields
}
