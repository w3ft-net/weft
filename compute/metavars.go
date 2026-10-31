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

// JqEnvelope carries a row's meta-variables next to its payload for a jq
// stage that reads them (see planner.JQMetaVars), and only the ones it reads.
// One jq process handles the whole row stream, so there's no per-row side
// channel; the row's own input is the only thing that varies. JqPerInput
// unwraps it and binds the variables, so the payload reaches the program as it
// is.
type JqEnvelope struct {
	vars []jqVar
}

// jqVar is a meta-variable: the jq name it binds and the row column it reads.
type jqVar struct {
	name, col string
	numeric   bool
}

// NewJqEnvelope is the envelope for a stage that reads these meta-variables.
func NewJqEnvelope(read planner.MetaRead) *JqEnvelope {
	e := &JqEnvelope{}
	for name, col := range planner.JQMetaVars {
		if read.Wants(col) {
			e.vars = append(e.vars, jqVar{name, col, planner.JQNumericMetaColumn(col)})
		}
	}
	return e
}

// Wrap is the payload with the row's meta-variables beside it. A payload that
// isn't JSON makes the envelope invalid, which jq reports as it would the bare
// payload; an empty one stays empty.
func (e *JqEnvelope) Wrap(payload string, meta map[string]string) string {
	if strings.TrimSpace(payload) == "" {
		return payload // no value for jq to read, as without the envelope
	}
	vars, _ := json.Marshal(e.values(meta))
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

// Vars is a row's meta-variables as jq reads them, for a caller that evaluates
// a jq program directly.
func (e *JqEnvelope) Vars(meta map[string]string) map[string]jq.Value {
	vars := make(map[string]jq.Value, len(e.vars))
	for name, v := range e.values(meta) {
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

// values maps each meta-variable the stage reads to its value: a string, a
// float64 for the numeric columns, or nil when the row has none.
func (e *JqEnvelope) values(meta map[string]string) map[string]any {
	vars := make(map[string]any, len(e.vars))
	for _, v := range e.vars {
		text, ok := meta[v.col]
		switch {
		case !ok:
			vars[v.name] = nil
		case v.numeric:
			if n, err := strconv.ParseFloat(text, 64); err == nil {
				vars[v.name] = n
			} else {
				vars[v.name] = nil
			}
		default:
			vars[v.name] = text
		}
	}
	return vars
}

// rowMetaVars is the meta-variables of a row that a stage reads: the columns
// it names, and ts, event_id and line_no, which every query selects as their
// own columns rather than projecting them as meta-variables, if it names
// those. The ts is DuckDB's CAST(ts AS VARCHAR), the form a SQL projection of
// $ts or a TS comparison sees.
func rowMetaVars(row *pb.ResultRow, read planner.MetaRead) map[string]string {
	meta := make(map[string]string, len(read.Columns())+3)
	if read.All {
		for k, v := range row.MetaVars {
			meta[k] = v
		}
	} else {
		for _, col := range read.Columns() {
			if v, ok := row.MetaVars[col]; ok {
				meta[col] = v
			}
		}
	}
	if read.Wants("ts") {
		meta["ts"] = FormatTS(row.TsUnixNs)
	}
	// A one-line event is line 0 of an event with no id. Both are always
	// set, since awk would otherwise keep the previous record's.
	if read.Wants("event_id") {
		meta["event_id"] = row.EventId
	}
	if read.Wants("line_no") {
		meta["line_no"] = strconv.Itoa(int(row.LineNo))
	}
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

// firstStageIsJq reports whether the job's first stage is jq, the one stage
// whose input carries the envelope.
func firstStageIsJq(stages []string) bool {
	return len(stages) > 0 && strings.HasPrefix(stages[0], "jq ")
}

// awkBinder expands a row's meta-variable columns into the awk names a stage
// reads: every alias that maps to a column it wants (both HOST and INSTANCE for
// the instance column) — the shape awk.Interpreter.PerRecordVars (via
// bash.RunStreamingWithAwkHook) expects. Unlike jq's envelope, this doesn't touch
// the record's own text: a bare reserved name is just an ordinary awk global
// once bound, no splicing into $0 needed or possible. See docs/known-issues.md's
// (fixed) awk meta-variable entry.
type awkBinder []awkAlias

type awkAlias struct{ name, col string }

func newAwkBinder(read planner.MetaRead) awkBinder {
	var b awkBinder
	for alias, col := range planner.AwkMetaVars {
		if read.Wants(col) {
			b = append(b, awkAlias{alias, col})
		}
	}
	return b
}

// vars is nil for a row with nothing to bind.
func (b awkBinder) vars(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	vars := make(map[string]string, len(b))
	for _, a := range b {
		if val, ok := meta[a.col]; ok {
			vars[a.name] = val
		}
	}
	return vars
}
