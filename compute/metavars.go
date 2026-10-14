package compute

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/jq"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// The envelope WrapJqInput puts around a payload, and JqPerInput takes off.
const (
	jqVarsKey  = "__weft_vars__"
	jqInputKey = "__weft_input__"
)

// WrapJqInput carries a row's meta-variables next to its payload for a
// jq stage that reads them (see planner.JQMetaVars). One jq process
// handles the whole row stream, so there's no per-row side channel; the
// row's own input is the only thing that varies. JqPerInput unwraps it
// and binds the variables, so the payload reaches the program as it is.
// A payload that isn't JSON makes the envelope invalid, which jq reports
// as it would the bare payload; an empty one stays empty.
func WrapJqInput(payload string, meta map[string]string) string {
	if strings.TrimSpace(payload) == "" {
		return payload // no value for jq to read, as without the envelope
	}
	vars, _ := json.Marshal(jqVars(meta))
	return `{"` + jqVarsKey + `":` + string(vars) + `,"` + jqInputKey + `":` + payload + `}`
}

// JqPerInput is jq.Options.PerInput (bash.StreamingOptions.JqPerInput)
// for inputs WrapJqInput made. Any other input, such as a later jq
// stage's, passes through unbound.
func JqPerInput(in jq.Value) (jq.Value, map[string]jq.Value) {
	if in.Type != jq.TypeObject || len(in.Obj) != 2 || in.Obj[0].Key != jqVarsKey {
		return in, nil
	}
	return in.ObjGet(jqInputKey), in.ObjGet(jqVarsKey).ObjMap()
}

// JqVars is a row's meta-variables as jq reads them, for a caller that
// evaluates a jq program directly.
func JqVars(meta map[string]string) map[string]jq.Value {
	vars := make(map[string]jq.Value, len(planner.JQMetaVars))
	for name, v := range jqVars(meta) {
		switch x := v.(type) {
		case string:
			vars[name] = jq.StrValue(x)
		case float64:
			vars[name] = jq.NumValue(x)
		default:
			vars[name] = jq.NullValue()
		}
	}
	return vars
}

// jqVars maps every jq meta-variable name to its value: a string, a
// float64 for the numeric columns, or nil when the row has none.
func jqVars(meta map[string]string) map[string]any {
	vars := make(map[string]any, len(planner.JQMetaVars))
	for name, col := range planner.JQMetaVars {
		text, ok := meta[col]
		switch {
		case !ok:
			vars[name] = nil
		case planner.JQNumericMetaColumn(col):
			if n, err := strconv.ParseFloat(text, 64); err == nil {
				vars[name] = n
			} else {
				vars[name] = nil
			}
		default:
			vars[name] = text
		}
	}
	return vars
}

// rowMetaVars is a row's meta-variable columns plus ts, event_id and
// line_no, which every query selects as their own columns rather than
// projecting them as meta-variables. Its text is DuckDB's CAST(ts AS VARCHAR), the form a
// SQL projection of $ts or a TS comparison sees.
func rowMetaVars(row *pb.ResultRow) map[string]string {
	meta := make(map[string]string, len(row.MetaVars)+1)
	for k, v := range row.MetaVars {
		meta[k] = v
	}
	meta["ts"] = FormatTS(row.TsUnixNs)
	// A one-line event is line 0 of an event with no id. Both are always
	// set, since awk would otherwise keep the previous record's.
	meta["event_id"] = row.EventId
	meta["line_no"] = strconv.Itoa(int(row.LineNo))
	return meta
}

// FormatTS renders a unix-nanosecond timestamp as DuckDB casts a
// TIMESTAMP to VARCHAR.
func FormatTS(unixNs int64) string {
	return time.Unix(0, unixNs).UTC().Format("2006-01-02 15:04:05.999999")
}

// firstStageIsAwk reports whether the job's first stage is awk, the one
// stage that reads shard rows and so the only one the per-record
// meta-var hook feeds (warp applies it only to an awk reading the
// pipeline's stdin). Any other shape skips the channel: nothing would
// drain it.
func firstStageIsAwk(stages []string) bool {
	return len(stages) > 0 && (stages[0] == "awk" || strings.HasPrefix(stages[0], "awk "))
}

// awkVarsFromMeta expands a row's projected meta-var columns (see
// planner.MetaVarColumns/AwkMetaVars) into every alias name that maps
// to a given column (e.g. both "HOST" and "INSTANCE" for the
// "instance" column) — the shape awk.Interpreter.PerRecordVars (via
// bash.RunStreamingWithAwkHook) expects. Unlike jq's MergeMetaVars,
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
