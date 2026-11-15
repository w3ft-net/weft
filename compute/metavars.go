package compute

import (
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/graybeardcorp/warp/pkg/jq"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// JqBinder binds a row's meta-variables for a jq stage that reads them (see
// planner.JQMetaVars), and only the ones it reads. The stage gets each row's
// variables from outside the stream (jq.Options.InputVars), so the payload
// reaches the program as it is.
type JqBinder struct {
	vars []jqVar
}

// jqVar is a meta-variable: the jq name it binds and the row column it reads.
type jqVar struct {
	name, col string
	numeric   bool
}

// NewJqBinder is the binder for a stage that reads these meta-variables.
func NewJqBinder(read planner.MetaRead) *JqBinder {
	e := &JqBinder{}
	for name, col := range planner.JQMetaVars {
		if read.Wants(col) {
			e.vars = append(e.vars, jqVar{name, col, planner.JQNumericMetaColumn(col)})
		}
	}
	return e
}

// Vars is a row's meta-variables as jq reads them, for a caller that evaluates
// a jq program directly.
func (e *JqBinder) Vars(meta map[string]string) map[string]jq.Value {
	vars := make(map[string]jq.Value, len(e.vars))
	for _, v := range e.vars {
		text, ok := meta[v.col]
		switch {
		case !ok:
			vars[v.name] = jq.NullValue()
		case v.numeric:
			if n, err := strconv.ParseFloat(text, 64); err == nil {
				vars[v.name] = jq.NumValue(n)
			} else {
				vars[v.name] = jq.NullValue()
			}
		default:
			vars[v.name] = jq.StrValue(text)
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

// bind appends a row's bindings for the names this stage reads to dst[:0], for
// awk.Interpreter.PerRecordBind. It reads the row directly, so a row costs no
// map; a column the row lacks is left unbound, as vars leaves it.
func (b awkBinder) bind(row *pb.ResultRow, dst []awk.Binding) []awk.Binding {
	dst = dst[:0]
	for _, a := range b {
		var val string
		ok := true
		switch a.col {
		case "ts":
			val = FormatTS(row.TsUnixNs)
		case "event_id":
			val = row.EventId
		case "line_no":
			val = strconv.Itoa(int(row.LineNo))
		default:
			val, ok = row.MetaVars[a.col]
		}
		if ok {
			dst = append(dst, awk.Binding{Name: a.name, Value: val})
		}
	}
	return dst
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
