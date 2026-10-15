package ingest

import (
	"fmt"
	"strconv"
	"time"
)

// RowDiff is one value that two schemas read differently from the same line.
type RowDiff struct {
	Line     int    // index into the lines compared
	Value    string // "timestamp", "level", "trace_id", ...
	Old, New string // what each schema read ("" for nothing)
}

// CompareSchemas parses every line with both schemas and returns each value
// they read differently. It is the check that makes changing a stored schema
// safe: an empty result over real lines means the two extract the same rows.
func CompareSchemas(old, new *Schema, lines []string) ([]RowDiff, error) {
	po, err := NewParser(old)
	if err != nil {
		return nil, fmt.Errorf("old schema: %w", err)
	}
	pn, err := NewParser(new)
	if err != nil {
		return nil, fmt.Errorf("new schema: %w", err)
	}
	var diffs []RowDiff
	for i, raw := range lines {
		ro, err := po.ParseLine(raw, probeSentinel)
		if err != nil {
			return nil, err
		}
		rn, err := pn.ParseLine(raw, probeSentinel)
		if err != nil {
			return nil, err
		}
		for _, v := range []struct{ name, o, n string }{
			{"timestamp", tsText(ro.TsUnixNs), tsText(rn.TsUnixNs)},
			{"level", ro.Level, rn.Level},
			{"trace_id", ro.TraceId, rn.TraceId},
			{"span_name", ro.SpanName, rn.SpanName},
			{"span_id", ro.SpanId, rn.SpanId},
			{"parent_span_id", ro.ParentSpanId, rn.ParentSpanId},
			{"duration_ns", strconv.FormatInt(ro.DurationNs, 10), strconv.FormatInt(rn.DurationNs, 10)},
			{"instance", ro.Instance, rn.Instance},
		} {
			if v.o != v.n {
				diffs = append(diffs, RowDiff{Line: i, Value: v.name, Old: zeroIsNothing(v.o), New: zeroIsNothing(v.n)})
			}
		}
	}
	return diffs, nil
}

// tsText renders a row's timestamp, saying so when it is just the ingest time
// (no timestamp was extracted).
func tsText(ns int64) string {
	if ns == probeSentinel.UnixNano() {
		return "(ingest time)"
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func zeroIsNothing(s string) string {
	if s == "0" {
		return ""
	}
	return s
}
