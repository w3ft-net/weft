package shellcore

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/weft/shellcore/chart"
)

// ChartPipeArgs holds the parsed flags for `... | chart` (pipe-stage form).
// Distinct from chartArgs (watch form) — the two parsers don't overlap.
// Exported so the non-interactive runner (shell/shell.go) can carry it
// across the query/render boundary.
type ChartPipeArgs struct {
	xSpec     FieldSpec
	ySpec     FieldSpec
	aggregate string        // count | sum | avg | min | max | p50 | p95 | identity
	bucket    time.Duration // 0 = no bucketing
	kind      string        // line | bar | sparkline
	title     string
}

// parseChartPipeArgs parses flag tokens passed to chart in pipe mode.
// The watch form (`chart <watch>`) routes to parseChartCmd instead.
//
// Defaults:
//   - aggregate: "count" when --bucket is set OR -y is absent;
//     "identity" otherwise (one row in, one point out).
//   - kind: "line" when --bucket is set, "bar" otherwise.
//
// Validation:
//   - -x and -y may not mix positional and JSON-path forms.
//   - aggregate must be a known reduction.
func parseChartPipeArgs(args []string) (ChartPipeArgs, string) {
	out := ChartPipeArgs{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-x":
			if i+1 >= len(args) {
				return out, "chart: -x requires a column index or .json.path"
			}
			s, err := ParseFieldSpec(args[i+1])
			if err != nil {
				return out, "chart: -x: " + err.Error()
			}
			out.xSpec = s
			i++
		case a == "-y":
			if i+1 >= len(args) {
				return out, "chart: -y requires a column index or .json.path"
			}
			s, err := ParseFieldSpec(args[i+1])
			if err != nil {
				return out, "chart: -y: " + err.Error()
			}
			out.ySpec = s
			i++
		case a == "-a" || a == "--aggregate":
			if i+1 >= len(args) {
				return out, "chart: -a requires a function (count|sum|avg|min|max|p50|p95)"
			}
			out.aggregate = args[i+1]
			i++
		case a == "--bucket":
			if i+1 >= len(args) {
				return out, "chart: --bucket requires a duration (e.g. 1m, 30s)"
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return out, fmt.Sprintf("chart: --bucket: invalid duration %q", args[i+1])
			}
			out.bucket = d
			i++
		case a == "-t" || a == "--type":
			if i+1 >= len(args) {
				return out, "chart: -t requires line|bar|sparkline"
			}
			out.kind = args[i+1]
			i++
		case a == "--title":
			if i+1 >= len(args) {
				return out, "chart: --title requires a value"
			}
			out.title = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			return out, fmt.Sprintf("chart: unknown flag %q", a)
		default:
			return out, fmt.Sprintf("chart: unexpected argument %q (use flags only in pipe mode)", a)
		}
	}

	// Mixing positional and JSON-path is rejected — the line parser
	// can't be in two modes at once.
	if !out.xSpec.IsZero() && !out.ySpec.IsZero() && out.xSpec.IsJSON() != out.ySpec.IsJSON() {
		return out, "chart: -x and -y must both be positional or both .json.path"
	}

	// Default kind: line when bucketing time, bar otherwise.
	if out.kind == "" {
		if out.bucket > 0 {
			out.kind = "line"
		} else {
			out.kind = "bar"
		}
	}
	switch out.kind {
	case "line", "bar", "sparkline":
	default:
		return out, fmt.Sprintf("chart: unknown type %q (want line|bar|sparkline)", out.kind)
	}

	// Default aggregate. With --bucket on, count is the most useful
	// reduction (rows per bucket). Without it each row is plotted
	// directly — identity preserves "the number on this line" for
	// the `wc -l | chart` case, where forcing count to 1 would lose
	// the upstream's actual value.
	if out.aggregate == "" {
		if out.bucket > 0 {
			out.aggregate = "count"
		} else {
			out.aggregate = "identity"
		}
	}
	if !validAggregate(out.aggregate) {
		return out, fmt.Sprintf("chart: unknown --aggregate %q", out.aggregate)
	}

	return out, ""
}

func validAggregate(s string) bool {
	switch s {
	case "identity", "count", "sum", "avg", "min", "max", "p50", "p95":
		return true
	}
	return false
}

// usesJSONLines reports whether stdin should be parsed as one JSON
// document per line. True when either x or y is a JSON-path spec.
func (a ChartPipeArgs) usesJSONLines() bool {
	return a.xSpec.IsJSON() || a.ySpec.IsJSON()
}

// SplitChartPipeline detects a `... | chart [flags]` suffix and returns
// the upstream pipeline together with the parsed chart args. Returns
// (line, nil, "") when the line has no chart pipe stage. Returns a
// non-empty errMsg only on flag-parse failure.
func SplitChartPipeline(line string) (weftPipeline string, args *ChartPipeArgs, errMsg string) {
	weft, tokens := splitTerminalStage(line)
	if len(tokens) == 0 || tokens[0] != "chart" {
		return line, nil, ""
	}
	parsed, msg := parseChartPipeArgs(tokens[1:])
	if msg != "" {
		return line, nil, msg
	}
	return weft, &parsed, ""
}

// RenderChartFromStream renders a pipe-stage chart given the raw output
// lines from the upstream pipeline. Returns (output, errMsg). errMsg is
// empty on success; otherwise a short string suitable for the shell to
// surface as `chart: <msg>`.
func RenderChartFromStream(lines []string, args ChartPipeArgs, w, h int) (string, string) {
	groups, ordered, isTimeX, errMsg := groupRows(lines, args)
	if errMsg != "" {
		return "", errMsg
	}
	if len(groups) == 0 {
		return "", "no data points to chart"
	}

	switch args.kind {
	case "sparkline":
		values := make([]float64, 0, len(ordered))
		for _, key := range ordered {
			values = append(values, reduce(args.aggregate, groups[key]))
		}
		return chart.Sparkline(values) + "\n", ""
	case "bar":
		labels := make([]string, 0, len(ordered))
		vals := make([]float64, 0, len(ordered))
		for _, key := range ordered {
			labels = append(labels, key.label)
			vals = append(vals, reduce(args.aggregate, groups[key]))
		}
		b := chart.Bar(w, h).BarValues(labels, vals)
		if args.title != "" {
			b = b.Title(args.title)
		}
		out, err := b.Render()
		if err != nil {
			return "", err.Error()
		}
		return out, ""
	default: // line
		pts := make([]chart.Point, 0, len(ordered))
		for _, key := range ordered {
			y := reduce(args.aggregate, groups[key])
			pts = append(pts, chart.Point{X: key.x, Y: y})
		}
		b := chart.Line(w, h).Points(pts)
		if args.title != "" {
			b = b.Title(args.title)
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

// groupKey is the per-group identity used to bucket rows. label is the
// rendered string form (used for bar X labels and as a stable map key);
// x is the numeric form fed to line charts. Both fields are populated
// even when only one will be used downstream.
type groupKey struct {
	label string
	x     float64
}

// groupRows walks the input lines, applies x/y extraction, and bins
// rows by their x value (with optional time bucketing). Returns the
// per-group y collections, the order of group keys (for stable
// rendering), and whether the x axis is a timestamp axis.
func groupRows(lines []string, args ChartPipeArgs) (map[groupKey][]float64, []groupKey, bool, string) {
	groups := map[groupKey][]float64{}
	var ordered []groupKey
	seen := map[string]groupKey{}

	isTimeX := args.bucket > 0
	// First pass to detect a time-shaped x when --bucket isn't set
	// but values look like timestamps. We sniff from the first row
	// only — heterogeneous x columns are a user error we don't try
	// to reason about.
	if !isTimeX && !args.xSpec.IsZero() {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			xv, err := ResolveField(args.xSpec, line)
			if err != nil || xv == "" {
				continue
			}
			if _, ok := parseTime(xv); ok {
				isTimeX = true
			}
			break
		}
	}

	seq := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// X resolution. With no -x, x is the row sequence (0, 1, 2…)
		// for line charts and a stringified seq for bar charts.
		var xLabel string
		var xNum float64
		if args.xSpec.IsZero() {
			xLabel = strconv.Itoa(seq)
			xNum = float64(seq)
		} else {
			raw, err := ResolveField(args.xSpec, line)
			if err != nil {
				return nil, nil, false, "x: " + err.Error()
			}
			if raw == "" {
				continue
			}
			if isTimeX {
				t, ok := parseTime(raw)
				if !ok {
					return nil, nil, false, fmt.Sprintf("x: cannot parse %q as a timestamp", raw)
				}
				if args.bucket > 0 {
					t = t.Truncate(args.bucket)
				}
				xLabel = formatBucketLabel(t, args.bucket)
				xNum = float64(t.Unix())
			} else if n, err := strconv.ParseFloat(raw, 64); err == nil {
				xLabel = raw
				xNum = n
			} else {
				xLabel = raw
				xNum = float64(seq) // categorical x → ordinal position
			}
		}

		// Y resolution. With no -y, y is the last whitespace field
		// of the line (compat with `wc -l | chart`).
		yStr := ""
		if args.ySpec.IsZero() {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				yStr = fields[len(fields)-1]
			}
		} else {
			raw, err := ResolveField(args.ySpec, line)
			if err != nil {
				return nil, nil, false, "y: " + err.Error()
			}
			yStr = raw
		}

		// "count" doesn't need a numeric y — every non-skipped row
		// contributes 1. Other reductions require numeric y.
		var yNum float64
		if args.aggregate == "count" {
			yNum = 1
		} else {
			if yStr == "" {
				continue
			}
			n, err := strconv.ParseFloat(yStr, 64)
			if err != nil {
				continue
			}
			yNum = n
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
	// order they appeared (or upstream-sort order, which the user
	// controls with `sort -k`).
	if isTimeX {
		sort.SliceStable(ordered, func(i, j int) bool {
			return ordered[i].x < ordered[j].x
		})
	}
	return groups, ordered, isTimeX, ""
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

// percentile returns the value at the given fraction (0..1) of a sorted
// copy of values using nearest-rank: the value at position
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
