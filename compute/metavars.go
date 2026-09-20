package compute

import (
	"encoding/json"
	"strings"

	"github.com/graybeardcorp/weft/planner"
)

// mergeMetaVars splices a row's projected meta-var columns (see
// planner.MetaVarColumns/JQMetaVars) into its raw JSON body as
// "_"-prefixed keys — every alias name that maps to a given column
// gets the same value (e.g. both "_host" and "_instance" for the
// "instance" column) — before the row reaches a jq compute stage.
//
// This exists because the jq stage runs as a literal program string
// piped raw text through warp's bash.RunStreaming: one process handles
// the whole row stream, so there's no per-row side channel (like a jq
// --arg) to hand the evaluator these values out of band. The row's own
// JSON body is the only thing that varies per row, so it's the only
// place a per-row value can actually reach the evaluator. See
// docs/known-issues.md's (fixed) jq meta-variable entry.
//
// Returns raw unchanged when there are no meta vars to inject, or when
// raw doesn't parse as a JSON object (plain text, an array, a bare
// scalar) — jq already behaves oddly against non-object input
// regardless of this fix, so there's nothing sensible to inject into.
// A real payload key is never overwritten: the underscore-prefix
// convention (docs/manual/metavars.md) exists specifically so these
// reserved keys don't collide with real producer data.
func mergeMetaVars(raw string, meta map[string]string) string {
	if len(meta) == 0 {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return raw
	}
	for alias, col := range planner.JQMetaVars {
		val, ok := meta[col]
		if !ok {
			continue
		}
		if _, exists := obj[alias]; exists {
			continue
		}
		encoded, err := json.Marshal(val)
		if err != nil {
			continue
		}
		obj[alias] = encoded
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return string(merged)
}

// stagesHaveAwk reports whether any compute stage is an awk
// invocation — used to decide whether a job needs the per-record
// meta-var channel/hook at all. The overwhelming common case (no awk
// stage) skips that machinery entirely.
func stagesHaveAwk(stages []string) bool {
	for _, s := range stages {
		if s == "awk" || strings.HasPrefix(s, "awk ") {
			return true
		}
	}
	return false
}

// awkVarsFromMeta expands a row's projected meta-var columns (see
// planner.MetaVarColumns/AwkMetaVars) into every alias name that maps
// to a given column (e.g. both "HOST" and "INSTANCE" for the
// "instance" column) — the shape awk.Interpreter.PerRecordVars (via
// bash.RunStreamingWithAwkHook) expects. Unlike jq's mergeMetaVars,
// this doesn't touch the record's own text: a bare reserved name is
// just an ordinary awk global once bound, no splicing into $0 needed
// or possible. See docs/known-issues.md's (fixed) awk meta-variable
// entry.
func awkVarsFromMeta(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	vars := make(map[string]string, len(planner.AwkMetaVars))
	for alias, col := range planner.AwkMetaVars {
		if val, ok := meta[col]; ok {
			vars[alias] = val
		}
	}
	return vars
}
