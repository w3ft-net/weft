package chart

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore/chart"
	"github.com/graybeardcorp/weft/internal/timerange"
)

// foldTop is how many tags a cron chart draws by name; the rest are
// summed into one "other" line so a high-cardinality cron stays legible.
const foldTop = 8

// otherTag and untaggedLabel label the folded line and the untagged
// series of a cron that also prints tags.
const (
	otherTag      = "other"
	untaggedLabel = "untagged"
)

// granularitySize is the bucket width of each storage tier; raw is 0.
var granularitySize = map[string]time.Duration{
	"raw":    0,
	"hourly": time.Hour,
	"4h":     4 * time.Hour,
	"daily":  24 * time.Hour,
}

// acc is the mergeable summary of a group of stored points. count, sum,
// min and max merge exactly at every tier; raw holds the values themselves
// and is only filled from raw runs, for the percentiles.
type acc struct {
	start      time.Time
	count      int64
	sum        float64
	min, max   float64
	raw        []float64
	hasExtrema bool
}

func (a *acc) add(b CronBucket, raw bool) {
	if !a.hasExtrema || b.Min < a.min {
		a.min = b.Min
	}
	if !a.hasExtrema || b.Max > a.max {
		a.max = b.Max
	}
	a.hasExtrema = true
	a.count += b.Count
	a.sum += b.Sum
	if raw {
		a.raw = append(a.raw, b.Sum)
	}
}

func (a *acc) merge(o *acc) {
	a.add(CronBucket{Count: o.count, Sum: o.sum, Min: o.min, Max: o.max}, false)
	a.raw = append(a.raw, o.raw...)
}

func (a *acc) value(aggregate string) float64 {
	switch aggregate {
	case "count":
		return float64(a.count)
	case "sum":
		return a.sum
	case "min":
		return a.min
	case "max":
		return a.max
	case "p50":
		return percentile(a.raw, 0.50)
	case "p95":
		return percentile(a.raw, 0.95)
	default: // avg
		return a.sum / float64(a.count)
	}
}

// cronLine is one tag's groups, oldest first.
type cronLine struct {
	tag  string
	accs []*acc
}

// cronLines groups a series per tag and per x position: the bucket start,
// floored to --bucket when one is given. Two runs of one tag at the same
// time (duplicate tags) land in one group. Tags past foldTop, ranked by
// their sum over the window, are merged into one "other" line.
func cronLines(series CronSeries, args WatchArgs) ([]cronLine, string) {
	if len(series.Buckets) == 0 {
		return nil, ""
	}
	tier, ok := granularitySize[series.Granularity]
	if !ok {
		return nil, fmt.Sprintf("unknown granularity %q", series.Granularity)
	}
	raw := series.Granularity == "raw"
	if (args.Aggregate == "p50" || args.Aggregate == "p95") && !raw {
		return nil, fmt.Sprintf("-a %s needs every run, and runs are kept 7 days; this range reads the %s rollups, which keep count, sum, min and max only", args.Aggregate, series.Granularity)
	}
	if args.Bucket > 0 && args.Bucket < tier {
		return nil, fmt.Sprintf("--bucket %s is finer than the %s rollups this range reads; use %s or more, or a shorter range", args.Bucket, series.Granularity, tier)
	}

	byTag := map[string]map[int64]*acc{}
	total := map[string]float64{}
	for _, b := range series.Buckets {
		x := b.Start
		if args.Bucket > 0 {
			x = b.Start.Truncate(args.Bucket)
		}
		groups := byTag[b.Tag]
		if groups == nil {
			groups = map[int64]*acc{}
			byTag[b.Tag] = groups
		}
		g := groups[x.UnixNano()]
		if g == nil {
			g = &acc{start: x}
			groups[x.UnixNano()] = g
		}
		g.add(b, raw)
		total[b.Tag] += b.Sum
	}

	tags := make([]string, 0, len(byTag))
	for t := range byTag {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if total[tags[i]] != total[tags[j]] {
			return total[tags[i]] > total[tags[j]]
		}
		return tags[i] < tags[j]
	})

	var lines []cronLine
	other := map[int64]*acc{}
	for i, t := range tags {
		if i < foldTop {
			lines = append(lines, cronLine{tag: t, accs: sortedAccs(byTag[t])})
			continue
		}
		for k, g := range byTag[t] {
			if o := other[k]; o != nil {
				o.merge(g)
			} else {
				c := *g
				c.raw = append([]float64(nil), g.raw...)
				other[k] = &c
			}
		}
	}
	// Name order reads better in a legend than rank order.
	sort.Slice(lines, func(i, j int) bool { return lines[i].tag < lines[j].tag })
	if len(other) > 0 {
		lines = append(lines, cronLine{tag: otherTag, accs: sortedAccs(other)})
	}
	return lines, ""
}

func sortedAccs(m map[int64]*acc) []*acc {
	out := make([]*acc, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out
}

// fitAccs merges consecutive groups so at most slots remain, for renderers
// with one column per point. Merging is exact for every aggregate.
func fitAccs(accs []*acc, slots int) (out []*acc, per int) {
	per = (len(accs) + slots - 1) / max(slots, 1)
	if per <= 1 {
		return accs, 1
	}
	for i := 0; i < len(accs); i += per {
		bin := &acc{start: accs[i].start}
		for _, a := range accs[i:min(i+per, len(accs))] {
			bin.merge(a)
		}
		out = append(out, bin)
	}
	return out, per
}

// CronPlot reduces a cron's series to the plot both renderers draw: one
// series per tag (untagged crons have one unnamed series), x in unix
// seconds, y the chart's aggregate.
func CronPlot(series CronSeries, args WatchArgs) (Plot, string) {
	lines, errMsg := cronLines(series, args)
	if errMsg != "" {
		return Plot{}, errMsg
	}
	plot := Plot{Kind: args.Kind, TimeX: true}
	if !series.Start.IsZero() && series.Start.Before(series.End) {
		plot.XMin, plot.XMax = float64(series.Start.Unix()), float64(series.End.Unix())
	}
	for _, l := range lines {
		s := PlotSeries{Tag: lineLabel(l.tag, len(lines))}
		for _, a := range l.accs {
			s.Points = append(s.Points, PlotPoint{X: float64(a.start.Unix()), Label: formatChartTime(float64(a.start.Unix())), Y: a.value(args.Aggregate)})
		}
		plot.Series = append(plot.Series, s)
	}
	return plot, ""
}

// lineLabel names a tag in a legend; the untagged series of a cron that
// also prints tags needs a name of its own.
func lineLabel(tag string, lines int) string {
	if tag == "" && lines > 1 {
		return untaggedLabel
	}
	return tag
}

// cronTitle is the default title: the cron, its window and its tier.
func cronTitle(name string, series CronSeries, args WatchArgs) string {
	if args.Title != "" {
		return args.Title
	}
	window := series.End.Sub(series.Start).Round(time.Minute)
	return fmt.Sprintf("%s  (%s of %s, %s)", name, args.Aggregate, formatWindow(window), series.Granularity)
}

func formatWindow(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	default:
		return strings.TrimSuffix(d.String(), "0s")
	}
}

// RenderCron draws one frame of a cron's chart sized to (w, h). Returns
// (output, errMsg); errMsg is empty on success. Both the one-shot builtin
// and the --follow View call it.
func RenderCron(name string, series CronSeries, args WatchArgs, w, h int) (string, string) {
	lines, errMsg := cronLines(series, args)
	if errMsg != "" {
		return "", errMsg
	}
	if len(lines) == 0 {
		return "", fmt.Sprintf("no numeric results for cron %q in this range", name)
	}
	title := cronTitle(name, series, args)

	switch args.Kind {
	case "sparkline":
		labelW := 0
		if len(lines) > 1 {
			for _, l := range lines {
				labelW = max(labelW, len(lineLabel(l.tag, len(lines))))
			}
			labelW++
		}
		var b strings.Builder
		for _, l := range lines {
			fitted, _ := fitAccs(l.accs, max(w-labelW, 1))
			values := make([]float64, len(fitted))
			for i, a := range fitted {
				values[i] = a.value(args.Aggregate)
			}
			if labelW > 0 {
				fmt.Fprintf(&b, "%-*s", labelW, lineLabel(l.tag, len(lines)))
			}
			b.WriteString(chart.Sparkline(values))
			b.WriteByte('\n')
		}
		return b.String(), ""
	case "bar":
		if len(lines) > 1 {
			return "", fmt.Sprintf("a bar chart draws one series and cron %q has %d tags; use -t line", name, len(lines))
		}
		fitted, per := fitAccs(lines[0].accs, chart.BarSlots(w))
		labels := make([]string, len(fitted))
		values := make([]float64, len(fitted))
		for i, a := range fitted {
			labels[i] = formatChartTime(float64(a.start.Unix()))
			values[i] = a.value(args.Aggregate)
		}
		footer := ""
		if per > 1 {
			footer = fmt.Sprintf("\x1b[2m%d points → %d bars, %d per bar (%s)\x1b[0m\n", len(lines[0].accs), len(fitted), per, args.Aggregate)
			h--
		}
		out, err := chart.Bar(w, h).Title(title).BarValues(labels, values).Render()
		if err != nil {
			return "", err.Error()
		}
		return out + footer, ""
	default: // line
		plot, _ := CronPlot(series, args)
		b := chart.Line(w, h).Title(title).XAxisTime().XRange(plot.XMin, plot.XMax)
		if len(plot.Series) == 1 {
			b = b.Points(chartPoints(plot.Series[0].Points))
		} else {
			named := make([]chart.NamedSeries, len(plot.Series))
			for i, s := range plot.Series {
				named[i] = chart.NamedSeries{Label: s.Tag, Points: chartPoints(s.Points)}
			}
			b = b.Series(named)
		}
		out, err := b.Render()
		if err != nil {
			return "", err.Error()
		}
		return out, ""
	}
}

// RenderOneShot fetches the cron's series over rangeSpec and renders one
// frame sized to (w, h). Both the interactive shell (cron-form chart
// command) and the non-interactive runner (weft sh -c "chart …") call
// this — the difference is only how the returned string is delivered.
func RenderOneShot(src CronSource, rangeSpec string, args WatchArgs, w, h int) (string, string) {
	series, err := src.GetCronSeries(args.Name, rangeSpec)
	if err != nil {
		return "", err.Error()
	}
	return RenderCron(args.Name, series, args, w, h)
}

// CheckFollow refuses --follow over a range that has already ended: an
// absolute end in the past has no new runs to wait for. A relative start
// slides on every redraw, and an absolute start with an open end grows.
func CheckFollow(rangeSpec string) string {
	_, end, err := timerange.ParseRange(rangeSpec)
	if err != nil {
		return "range: " + err.Error()
	}
	if time.Until(end) < -time.Minute {
		return fmt.Sprintf("--follow needs a range that runs to now, and %q ended at %s", rangeSpec, end.Format(time.RFC3339))
	}
	return ""
}

// formatChartTime is the bar-mode X-axis label formatter — uses
// the same HH:MM convention as the line-chart time axis so
// visually-similar data shows visually-similar labels.
func formatChartTime(epochSecs float64) string {
	t := time.Unix(int64(epochSecs), 0)
	if t.Second() == 0 {
		return t.Format("15:04")
	}
	return t.Format("15:04:05")
}
