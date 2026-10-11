package chart

// Plot is a pipe-form chart reduced to plain data: the grouped, bucketed and
// aggregated points a renderer draws. The terminal renderer (RenderFromStream)
// and the browser modal both start from it, so `-x`/`-y`/`--bucket`/`-a`/`--tag`
// mean the same thing on either surface.
type Plot struct {
	Kind   string       `json:"kind"`   // line | bar | sparkline
	TimeX  bool         `json:"timeX"`  // X is unix seconds
	Series []PlotSeries `json:"series"` // exactly one unnamed series unless --tag was given
}

// PlotSeries is one line (or the single bar group). Tag is empty for an
// untagged chart.
type PlotSeries struct {
	Tag    string      `json:"tag"`
	Points []PlotPoint `json:"points"`
}

// PlotPoint is one reduced group: X is the numeric position (unix seconds for
// a time axis), Label its rendered form, Y the aggregate.
type PlotPoint struct {
	X     float64 `json:"x"`
	Label string  `json:"label"`
	Y     float64 `json:"y"`
}

// PlotFromStream groups the upstream lines per args and reduces each group.
// errMsg is empty on success, otherwise a short string for `chart: <msg>`.
func PlotFromStream(lines []string, args PipeArgs) (Plot, string) {
	plot := Plot{Kind: args.Kind}

	if !args.TagSpec.IsZero() {
		tagGroups, isTimeX, errMsg := groupRowsByTag(lines, args)
		if errMsg != "" {
			return Plot{}, errMsg
		}
		if len(tagGroups) == 0 {
			return Plot{}, "no data points to chart"
		}
		plot.TimeX = isTimeX
		for _, tg := range tagGroups {
			plot.Series = append(plot.Series, PlotSeries{Tag: tg.tag, Points: reduceGroups(tg.groups, tg.ordered, args.Aggregate)})
		}
		return plot, ""
	}

	groups, ordered, isTimeX, errMsg := groupRows(lines, args)
	if errMsg != "" {
		return Plot{}, errMsg
	}
	if len(groups) == 0 {
		return Plot{}, "no data points to chart"
	}
	plot.TimeX = isTimeX
	plot.Series = []PlotSeries{{Points: reduceGroups(groups, ordered, args.Aggregate)}}
	return plot, ""
}

func reduceGroups(groups map[groupKey][]float64, ordered []groupKey, aggregate string) []PlotPoint {
	pts := make([]PlotPoint, 0, len(ordered))
	for _, key := range ordered {
		pts = append(pts, PlotPoint{X: key.x, Label: key.label, Y: reduce(aggregate, groups[key])})
	}
	return pts
}
