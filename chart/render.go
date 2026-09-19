package chart

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/warp/pkg/shellcore/chart"
)

// RenderFromStream renders a pipe-stage chart from the upstream
// pipeline's output lines. Returns (output, errMsg). errMsg is
// empty on success; otherwise a short string suitable for the
// shell to surface as `chart: <msg>`.
//
// Exported so the non-interactive runner (weft sh -c "...") can
// call it directly without going through shellcore's pipe-stage
// dispatch.
func RenderFromStream(lines []string, args PipeArgs, w, h int) (string, string) {
	if !args.TagSpec.IsZero() {
		return renderTagged(lines, args, w, h)
	}

	groups, ordered, isTimeX, errMsg := groupRows(lines, args)
	if errMsg != "" {
		return "", errMsg
	}
	if len(groups) == 0 {
		return "", "no data points to chart"
	}

	switch args.Kind {
	case "sparkline":
		values := make([]float64, 0, len(ordered))
		for _, key := range ordered {
			values = append(values, reduce(args.Aggregate, groups[key]))
		}
		return chart.Sparkline(values) + "\n", ""
	case "bar":
		labels := make([]string, 0, len(ordered))
		vals := make([]float64, 0, len(ordered))
		for _, key := range ordered {
			labels = append(labels, key.label)
			vals = append(vals, reduce(args.Aggregate, groups[key]))
		}
		b := chart.Bar(w, h).BarValues(labels, vals)
		if args.Title != "" {
			b = b.Title(args.Title)
		}
		out, err := b.Render()
		if err != nil {
			return "", err.Error()
		}
		return out, ""
	default: // line
		pts := make([]chart.Point, 0, len(ordered))
		for _, key := range ordered {
			y := reduce(args.Aggregate, groups[key])
			pts = append(pts, chart.Point{X: key.x, Y: y})
		}
		b := chart.Line(w, h).Points(pts)
		if args.Title != "" {
			b = b.Title(args.Title)
		}
		if isTimeX {
			b = b.XAxisTime()
		}
		out, err := b.Render()
		if err != nil {
			return "", err.Error()
		}
		return out, ""
	}
}

// renderTagged is RenderFromStream's --tag path: each row is first
// split by its tag value, then bucketed by x independently within that
// tag (via groupRowsByTag, which otherwise applies the exact same x/y
// rules as the untagged path). One warp chart.NamedSeries comes out
// per distinct tag value, feeding a single multi-series line chart.
//
// ParsePipeArgs already rejects --tag combined with anything but
// -t line, so this never needs to branch on args.Kind.
func renderTagged(lines []string, args PipeArgs, w, h int) (string, string) {
	tagGroups, isTimeX, errMsg := groupRowsByTag(lines, args)
	if errMsg != "" {
		return "", errMsg
	}
	if len(tagGroups) == 0 {
		return "", "no data points to chart"
	}

	series := make([]chart.NamedSeries, 0, len(tagGroups))
	for _, tg := range tagGroups {
		pts := make([]chart.Point, 0, len(tg.ordered))
		for _, key := range tg.ordered {
			pts = append(pts, chart.Point{X: key.x, Y: reduce(args.Aggregate, tg.groups[key])})
		}
		series = append(series, chart.NamedSeries{Label: tg.tag, Points: pts})
	}

	b := chart.Line(w, h).Series(series)
	if args.Title != "" {
		b = b.Title(args.Title)
	}
	if isTimeX {
		b = b.XAxisTime()
	}
	out, err := b.Render()
	if err != nil {
		return "", err.Error()
	}
	return out, ""
}

// groupKey is the per-group identity used to bucket rows. label is
// the rendered string form (used for bar X labels and as a stable
// map key); x is the numeric form fed to line charts. Both fields
// are populated even when only one will be used downstream.
type groupKey struct {
	label string
	x     float64
}

// rowStatus is resolveXY's per-row outcome: rowOK means xLabel/xNum/yNum
// are populated and the row contributes to a group; rowSkip means the
// row is silently dropped (e.g. missing/non-numeric field — normal for
// noisy log data); rowError means the row indicates a hard problem
// (malformed field-path resolution, an unparseable timestamp) that
// should abort the whole render rather than silently produce a
// misleading partial chart.
type rowStatus int

const (
	rowOK rowStatus = iota
	rowSkip
	rowError
)

// resolveXY applies the shared x/y extraction rules — used by both the
// untagged (groupRows) and tagged (groupRowsByTag) grouping paths, so
// a row is bucketed identically either way; only the tag adds a second
// dimension around this.
func resolveXY(line string, args PipeArgs, isTimeX bool, seq int) (xLabel string, xNum, yNum float64, status rowStatus, errMsg string) {
	// X resolution. With no -x, x is the row sequence.
	if args.XSpec.IsZero() {
		xLabel = strconv.Itoa(seq)
		xNum = float64(seq)
	} else {
		raw, err := shellcore.ResolveField(args.XSpec, line)
		if err != nil {
			return "", 0, 0, rowError, "x: " + err.Error()
		}
		if raw == "" {
			return "", 0, 0, rowSkip, ""
		}
		if isTimeX {
			t, ok := parseTime(raw)
			if !ok {
				return "", 0, 0, rowError, fmt.Sprintf("x: cannot parse %q as a timestamp", raw)
			}
			if args.Bucket > 0 {
				t = t.Truncate(args.Bucket)
			}
			xLabel = formatBucketLabel(t, args.Bucket)
			xNum = float64(t.Unix())
		} else if n, err := strconv.ParseFloat(raw, 64); err == nil {
			xLabel = raw
			xNum = n
		} else {
			xLabel = raw
			xNum = float64(seq) // categorical x → ordinal position
		}
	}

	// Y resolution. With no -y, y is the last whitespace field.
	yStr := ""
	if args.YSpec.IsZero() {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			yStr = fields[len(fields)-1]
		}
	} else {
		raw, err := shellcore.ResolveField(args.YSpec, line)
		if err != nil {
			return "", 0, 0, rowError, "y: " + err.Error()
		}
		yStr = raw
	}

	// "count" doesn't need a numeric y — every non-skipped row
	// contributes 1. Other reductions require numeric y.
	if args.Aggregate == "count" {
		yNum = 1
	} else {
		if yStr == "" {
			return "", 0, 0, rowSkip, ""
		}
		n, err := strconv.ParseFloat(yStr, 64)
		if err != nil {
			return "", 0, 0, rowSkip, ""
		}
		yNum = n
	}
	return xLabel, xNum, yNum, rowOK, ""
}

// detectTimeX decides whether the x axis is a timestamp: always true
// with --bucket set; otherwise sniffed from the first row that
// resolves a non-empty -x value. Heterogeneous x columns are a user
// error this doesn't try to reason about.
func detectTimeX(lines []string, args PipeArgs) bool {
	if args.Bucket > 0 {
		return true
	}
	if args.XSpec.IsZero() {
		return false
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		xv, err := shellcore.ResolveField(args.XSpec, line)
		if err != nil || xv == "" {
			continue
		}
		_, ok := parseTime(xv)
		return ok
	}
	return false
}

// groupRows walks the input lines, applies x/y extraction, and
// bins rows by their x value (with optional time bucketing).
// Returns the per-group y collections, the order of group keys
// (for stable rendering), and whether the x axis is a timestamp.
func groupRows(lines []string, args PipeArgs) (map[groupKey][]float64, []groupKey, bool, string) {
	groups := map[groupKey][]float64{}
	var ordered []groupKey
	seen := map[string]groupKey{}
	isTimeX := detectTimeX(lines, args)

	seq := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		xLabel, xNum, yNum, status, errMsg := resolveXY(line, args, isTimeX, seq)
		if status == rowError {
			return nil, nil, false, errMsg
		}
		if status == rowSkip {
			continue
		}

		key, ok := seen[xLabel]
		if !ok {
			key = groupKey{label: xLabel, x: xNum}
			seen[xLabel] = key
			ordered = append(ordered, key)
		}
		groups[key] = append(groups[key], yNum)
		seq++
	}

	// For line/time charts, ordering must be by x ascending so the
	// renderer reads left-to-right as time-forward. For bar charts
	// we keep insertion order so categorical inputs render in the
	// order they appeared.
	if isTimeX {
		sort.SliceStable(ordered, func(i, j int) bool {
			return ordered[i].x < ordered[j].x
		})
	}
	return groups, ordered, isTimeX, ""
}

// tagGroup is one tag value's grouped rows — the same shape groupRows
// produces for the whole (untagged) input, scoped to rows matching one
// tag value.
type tagGroup struct {
	tag     string
	groups  map[groupKey][]float64
	ordered []groupKey
}

// groupRowsByTag is groupRows with an added split dimension: each row
// is first assigned to a tag via args.TagSpec (rows with a missing or
// unresolvable tag are dropped, same as a missing x/y field), then
// bucketed by x independently within that tag using the identical
// resolveXY rules groupRows uses. Returns one tagGroup per distinct tag
// value, ordered alphabetically by tag (a stable, predictable legend
// and color order — not input order, which would depend on which tag's
// rows happened to come first in a given query result).
func groupRowsByTag(lines []string, args PipeArgs) ([]tagGroup, bool, string) {
	isTimeX := detectTimeX(lines, args)

	// seq is per-tag, not global: two coordinators' rows are typically
	// interleaved in a query result, and a shared counter would scatter
	// each series' ordinal x-fallback across the interleaving order
	// instead of giving each series its own clean 0,1,2,....
	type tagState struct {
		groups  map[groupKey][]float64
		ordered []groupKey
		seen    map[string]groupKey
		seq     int
	}
	states := map[string]*tagState{}
	var tagOrder []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		tagVal, err := shellcore.ResolveField(args.TagSpec, line)
		if err != nil {
			return nil, false, "tag: " + err.Error()
		}
		if tagVal == "" {
			continue
		}

		st, exists := states[tagVal]
		if !exists {
			st = &tagState{groups: map[groupKey][]float64{}, seen: map[string]groupKey{}}
			states[tagVal] = st
			tagOrder = append(tagOrder, tagVal)
		}

		xLabel, xNum, yNum, status, errMsg := resolveXY(line, args, isTimeX, st.seq)
		if status == rowError {
			return nil, false, errMsg
		}
		if status == rowSkip {
			continue
		}

		key, ok := st.seen[xLabel]
		if !ok {
			key = groupKey{label: xLabel, x: xNum}
			st.seen[xLabel] = key
			st.ordered = append(st.ordered, key)
		}
		st.groups[key] = append(st.groups[key], yNum)
		st.seq++
	}

	sort.Strings(tagOrder)
	out := make([]tagGroup, 0, len(tagOrder))
	for _, tag := range tagOrder {
		st := states[tag]
		if isTimeX {
			sort.SliceStable(st.ordered, func(i, j int) bool {
				return st.ordered[i].x < st.ordered[j].x
			})
		}
		out = append(out, tagGroup{tag: tag, groups: st.groups, ordered: st.ordered})
	}
	return out, isTimeX, ""
}

// reduce applies the named aggregate to a slice of values.
func reduce(name string, values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	switch name {
	case "count":
		return float64(len(values))
	case "sum":
		s := 0.0
		for _, v := range values {
			s += v
		}
		return s
	case "avg":
		s := 0.0
		for _, v := range values {
			s += v
		}
		return s / float64(len(values))
	case "min":
		m := values[0]
		for _, v := range values[1:] {
			if v < m {
				m = v
			}
		}
		return m
	case "max":
		m := values[0]
		for _, v := range values[1:] {
			if v > m {
				m = v
			}
		}
		return m
	case "p50":
		return percentile(values, 0.50)
	case "p95":
		return percentile(values, 0.95)
	default: // identity
		return values[0]
	}
}

// percentile returns the value at the given fraction (0..1) of a
// sorted copy of values using nearest-rank: the value at position
// ceil(p * n), 1-indexed, clamped to the slice. Adequate for chart
// rendering; not an exact statistical percentile.
func percentile(values []float64, p float64) float64 {
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	idx := int(math.Ceil(p*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// parseTime accepts RFC3339 or epoch seconds (integer or float).
// Returns the parsed time and ok=true on success.
func parseTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		// epoch seconds — accept floats with sub-second precision
		secs := int64(n)
		nsecs := int64((n - float64(secs)) * 1e9)
		return time.Unix(secs, nsecs), true
	}
	return time.Time{}, false
}

// formatBucketLabel renders a bucket key. With bucketing on, drop
// sub-bucket precision so the label matches the bucket boundary.
func formatBucketLabel(t time.Time, bucket time.Duration) string {
	switch {
	case bucket >= time.Hour:
		return t.Format("2006-01-02 15:04")
	case bucket >= time.Minute:
		return t.Format("15:04")
	default:
		return t.Format("15:04:05")
	}
}
