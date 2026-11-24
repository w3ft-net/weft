package chart

import (
	"fmt"
	"github.com/w3ft-net/weft/fieldspec"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/timefmt-go"
	"github.com/w3ft-net/weft/chart/termchart"
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
	if args.Kind == "line" {
		plot, errMsg := PlotFromStream(lines, args)
		if errMsg != "" {
			return "", errMsg
		}
		b := termchart.Line(w, h).XRange(plot.XMin, plot.XMax)
		if len(plot.Series) > 1 || plot.Series[0].Tag != "" {
			series := make([]termchart.NamedSeries, 0, len(plot.Series))
			for _, s := range plot.Series {
				series = append(series, termchart.NamedSeries{Label: s.Tag, Points: chartPoints(s.Points)})
			}
			return renderLine(b.Series(series), args, plot.TimeX)
		}
		return renderLine(b.Points(chartPoints(plot.Series[0].Points)), args, plot.TimeX)
	}

	// Bar and sparkline draw one cell column per point, so unlike a line
	// (which maps x onto the braille canvas) they must be binned to the
	// width up front — from the raw rows, while the aggregate still can
	// be re-applied. ParsePipeArgs rejects --tag for both, so the input is
	// always a single series.
	groups, ordered, _, errMsg := groupRows(lines, args)
	if errMsg != "" {
		return "", errMsg
	}
	if len(groups) == 0 {
		return "", "no data points to chart"
	}
	slots := w
	if args.Kind == "bar" {
		slots = termchart.BarSlots(w)
	}
	pts, per := fitPoints(groups, ordered, args.Aggregate, slots)
	values := make([]float64, 0, len(pts))
	for _, p := range pts {
		values = append(values, p.Y)
	}
	if args.Kind == "sparkline" {
		return termchart.Sparkline(values) + "\n", ""
	}

	labels := make([]string, 0, len(pts))
	for _, p := range pts {
		labels = append(labels, p.Label)
	}
	footer := ""
	if per > 1 {
		agg := args.Aggregate
		if agg == "identity" {
			agg = "avg"
		}
		footer = fmt.Sprintf("\x1b[2m%d points → %d bars, %d per bar (%s)\x1b[0m\n", len(ordered), len(pts), per, agg)
		h--
	}
	b := termchart.Bar(w, h).BarValues(labels, values)
	if args.Title != "" {
		b = b.Title(args.Title)
	}
	out, err := b.Render()
	if err != nil {
		return "", err.Error()
	}
	return out + footer, ""
}

func chartPoints(pts []PlotPoint) []termchart.Point {
	out := make([]termchart.Point, len(pts))
	for i, p := range pts {
		out[i] = termchart.Point{X: p.X, Y: p.Y}
	}
	return out
}

func renderLine(b *termchart.Builder, args PipeArgs, timeX bool) (string, string) {
	if args.Title != "" {
		b = b.Title(args.Title)
	}
	if timeX {
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
		raw, err := resolveX(args, line)
		if err != nil {
			return "", 0, 0, rowError, "x: " + err.Error()
		}
		if raw == "" {
			return "", 0, 0, rowSkip, ""
		}
		if isTimeX {
			t, ok := parseTime(raw, args.XFormat)
			if !ok && args.XFormat != "" {
				return "", 0, 0, rowError, fmt.Sprintf("x: %q does not match %s", raw, args.XFormat)
			}
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

	// Y resolution. With no -y, y is the first whitespace field that -x
	// doesn't read: the value of a cron's `<value> [<tag>]` line, so a
	// line reads the same piped into chart and stored by a cron.
	yStr := ""
	if args.YSpec.IsZero() {
		col := 1
		if args.XSpec.Index == 1 {
			col = max(args.XEnd, 1) + 1
		}
		if fields := strings.Fields(line); len(fields) >= col {
			yStr = fields[col-1]
		}
	} else {
		raw, err := fieldspec.ResolveField(args.YSpec, line)
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

// resolveX is fieldspec.ResolveField for -x, plus the column range a
// ::time cast allows: columns start..end joined by single spaces, or
// "" when the line is too short to have them all.
func resolveX(args PipeArgs, line string) (string, error) {
	if args.XEnd == 0 {
		return fieldspec.ResolveField(args.XSpec, line)
	}
	fields := strings.Fields(line)
	if len(fields) < args.XEnd {
		return "", nil
	}
	return strings.Join(fields[args.XSpec.Index-1:args.XEnd], " "), nil
}

// detectTimeX decides whether the x axis is a timestamp: always true
// with --bucket or a ::time cast; otherwise sniffed from the first row
// that resolves a non-empty -x value, which only an RFC3339 stamp
// passes — a bare number is far likelier a value (an HHMM minute, a
// count) than epoch seconds, and `::time` says so when it isn't.
// Heterogeneous x columns are a user error this doesn't try to reason
// about.
func detectTimeX(lines []string, args PipeArgs) bool {
	if args.Bucket > 0 || args.XTime {
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
		xv, err := resolveX(args, line)
		if err != nil || xv == "" {
			continue
		}
		_, ok := parseRFC3339(xv)
		return ok
	}
	return false
}

// groupID is the identity rows group under: the label, except on a
// time axis, where it's the instant — a sub-hour bucket's label
// ("15:04") omits the date, so the same minute of two days would merge.
func groupID(label string, x float64, isTimeX bool) string {
	if isTimeX {
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return label
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

		id := groupID(xLabel, xNum, isTimeX)
		key, ok := seen[id]
		if !ok {
			key = groupKey{label: xLabel, x: xNum}
			seen[id] = key
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
	if zeroFills(args) && len(ordered) > 0 {
		var errMsg string
		lo, hi := args.span(ordered[0].x, ordered[len(ordered)-1].x)
		ordered, errMsg = fillEmptyBuckets(groups, ordered, args, lo, hi)
		if errMsg != "" {
			return nil, nil, false, errMsg
		}
	}
	return groups, ordered, isTimeX, ""
}

// maxFilledBuckets bounds zero-filling: a --bucket that fine for the
// data's span is more buckets than any chart can draw.
const maxFilledBuckets = 100_000

// zeroFills reports whether empty --bucket buckets are drawn as 0:
// only count and sum have a meaningful zero (no rows → nothing
// counted); an avg/min/max/percentile of no rows is undefined, so
// those leave empty buckets out.
func zeroFills(args PipeArgs) bool {
	return args.Bucket > 0 && (args.Aggregate == "count" || args.Aggregate == "sum")
}

// fillEmptyBuckets adds a row-less group for every --bucket step in
// [lo, hi] (unix seconds, floored to bucket starts) that has none, so a
// stretch with no logs draws as a hole at 0 instead of the line bridging
// it. Returns the x-sorted keys including the filled ones.
func fillEmptyBuckets(groups map[groupKey][]float64, ordered []groupKey, args PipeArgs, lo, hi float64) ([]groupKey, string) {
	start := time.Unix(int64(lo), 0).Truncate(args.Bucket)
	end := time.Unix(int64(hi), 0).Truncate(args.Bucket)
	span := end.Sub(start)
	if n := int64(span/args.Bucket) + 1; n > maxFilledBuckets {
		return nil, fmt.Sprintf("--bucket %s over %s is %d buckets (max %d); use a wider --bucket", args.Bucket, span, n, maxFilledBuckets)
	}
	have := make(map[float64]bool, len(ordered))
	for _, key := range ordered {
		have[key.x] = true
	}
	for t := start; !t.After(end); t = t.Add(args.Bucket) {
		x := float64(t.Unix())
		if have[x] {
			continue
		}
		key := groupKey{label: formatBucketLabel(t, args.Bucket), x: x}
		groups[key] = nil
		ordered = append(ordered, key)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].x < ordered[j].x
	})
	return ordered, ""
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

		tagVal, err := fieldspec.ResolveField(args.TagSpec, line)
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

		id := groupID(xLabel, xNum, isTimeX)
		key, ok := st.seen[id]
		if !ok {
			key = groupKey{label: xLabel, x: xNum}
			st.seen[id] = key
			st.ordered = append(st.ordered, key)
		}
		st.groups[key] = append(st.groups[key], yNum)
		st.seq++
	}

	// Every series is filled over the whole chart's span, not just its
	// own, so a tag that went quiet before another started shows zeros.
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, st := range states {
		for _, key := range st.ordered {
			lo, hi = min(lo, key.x), max(hi, key.x)
		}
	}
	if isTimeX {
		lo, hi = args.span(lo, hi)
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
		if zeroFills(args) && len(st.ordered) > 0 {
			var errMsg string
			st.ordered, errMsg = fillEmptyBuckets(st.groups, st.ordered, args, lo, hi)
			if errMsg != "" {
				return nil, false, errMsg
			}
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

// parseTime parses s with a strftime format when one is given (a
// zone-less stamp is UTC); otherwise it accepts RFC3339, the zone-less
// "2006-01-02 15:04:05[.ffffff]" form that `$ts` prints (UTC), or epoch
// seconds (integer or float). Returns the parsed time and ok=true on
// success.
func parseTime(s, format string) (time.Time, bool) {
	if format != "" {
		t, err := timefmt.Parse(s, format)
		return t, err == nil
	}
	if t, ok := parseRFC3339(s); ok {
		return t, true
	}
	// Go reads a fractional second after the seconds field even when the
	// layout has none, so this takes the stamp with or without one.
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
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

func parseRFC3339(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// formatBucketLabel renders a bucket key in local time, matching the
// line chart's time axis. With bucketing on, drop
// sub-bucket precision so the label matches the bucket boundary.
func formatBucketLabel(t time.Time, bucket time.Duration) string {
	t = t.Local()
	switch {
	case bucket >= time.Hour:
		return t.Format("2006-01-02 15:04")
	case bucket >= time.Minute:
		return t.Format("15:04")
	default:
		return t.Format("15:04:05")
	}
}
