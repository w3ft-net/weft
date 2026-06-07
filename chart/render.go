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

// groupKey is the per-group identity used to bucket rows. label is
// the rendered string form (used for bar X labels and as a stable
// map key); x is the numeric form fed to line charts. Both fields
// are populated even when only one will be used downstream.
type groupKey struct {
	label string
	x     float64
}

// groupRows walks the input lines, applies x/y extraction, and
// bins rows by their x value (with optional time bucketing).
// Returns the per-group y collections, the order of group keys
// (for stable rendering), and whether the x axis is a timestamp.
func groupRows(lines []string, args PipeArgs) (map[groupKey][]float64, []groupKey, bool, string) {
	groups := map[groupKey][]float64{}
	var ordered []groupKey
	seen := map[string]groupKey{}

	isTimeX := args.Bucket > 0
	// First pass to detect a time-shaped x when --bucket isn't set
	// but values look like timestamps. Sniff from the first row only —
	// heterogeneous x columns are a user error we don't reason about.
	if !isTimeX && !args.XSpec.IsZero() {
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			xv, err := shellcore.ResolveField(args.XSpec, line)
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

		// X resolution. With no -x, x is the row sequence.
		var xLabel string
		var xNum float64
		if args.XSpec.IsZero() {
			xLabel = strconv.Itoa(seq)
			xNum = float64(seq)
		} else {
			raw, err := shellcore.ResolveField(args.XSpec, line)
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
				return nil, nil, false, "y: " + err.Error()
			}
			yStr = raw
		}

		// "count" doesn't need a numeric y — every non-skipped row
		// contributes 1. Other reductions require numeric y.
		var yNum float64
		if args.Aggregate == "count" {
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
	// order they appeared.
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
