package chart

import (
	"encoding/json"
	"math"
)

// Plot is a pipe-form chart reduced to plain data: the grouped, bucketed and
// aggregated points a renderer draws. The terminal renderer (RenderFromStream)
// and the browser modal both start from it, so `-x`/`-y`/`--bucket`/`-a`/`--tag`
// mean the same thing on either surface.
type Plot struct {
	Kind   string       `json:"kind"`   // line | bar | sparkline
	TimeX  bool         `json:"timeX"`  // X is unix seconds
	Series []PlotSeries `json:"series"` // exactly one unnamed series unless --tag was given
	// XMin/XMax fix a time axis to the shell's range (widened to any data
	// outside it), so quiet stretches at either end show as such; both 0
	// means the data's own span.
	XMin float64 `json:"xMin,omitempty"`
	XMax float64 `json:"xMax,omitempty"`
}

// MarshalJSON sends no series as [], not null: the browser maps over it.
func (p Plot) MarshalJSON() ([]byte, error) {
	type plot Plot
	if p.Series == nil {
		p.Series = []PlotSeries{}
	}
	return json.Marshal(plot(p))
}

// fixTimeSpan sets XMin/XMax from the range in args and the data's extent.
func (p *Plot) fixTimeSpan(args PipeArgs) {
	if !p.TimeX || args.RangeStart.IsZero() {
		return
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range p.Series {
		for _, pt := range s.Points {
			lo, hi = min(lo, pt.X), max(hi, pt.X)
		}
	}
	p.XMin, p.XMax = args.span(lo, hi)
}

// PlotSeries is one line (or the single bar group). Tag is empty for an
// untagged chart.
type PlotSeries struct {
	Tag    string      `json:"tag"`
	Points []PlotPoint `json:"points"`
}

// MarshalJSON sends no points as [], not null: the browser maps over it.
func (s PlotSeries) MarshalJSON() ([]byte, error) {
	type series PlotSeries
	if s.Points == nil {
		s.Points = []PlotPoint{}
	}
	return json.Marshal(series(s))
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
		plot.fixTimeSpan(args)
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
	plot.fixTimeSpan(args)
	return plot, ""
}

func reduceGroups(groups map[groupKey][]float64, ordered []groupKey, aggregate string) []PlotPoint {
	pts := make([]PlotPoint, 0, len(ordered))
	for _, key := range ordered {
		pts = append(pts, PlotPoint{X: key.x, Label: key.label, Y: reduce(aggregate, groups[key])})
	}
	return pts
}

// fitPoints is reduceGroups for a renderer with only slots columns:
// consecutive groups are merged per points so the result has at most
// slots points, each labelled (and positioned) by its first group.
// A merged bin is re-reduced from its raw rows with the chart's own
// aggregate, so count/sum add up, min/max keep dips/spikes, and
// p50/p95 are the bin's true percentiles; identity, which has no
// reduction, averages its groups' values.
func fitPoints(groups map[groupKey][]float64, ordered []groupKey, aggregate string, slots int) (pts []PlotPoint, per int) {
	per = (len(ordered) + slots - 1) / max(slots, 1)
	if per <= 1 {
		return reduceGroups(groups, ordered, aggregate), 1
	}
	for i := 0; i < len(ordered); i += per {
		bin := ordered[i:min(i+per, len(ordered))]
		var raw []float64
		agg := aggregate
		for _, key := range bin {
			if aggregate == "identity" {
				raw = append(raw, reduce("identity", groups[key]))
				agg = "avg"
			} else {
				raw = append(raw, groups[key]...)
			}
		}
		pts = append(pts, PlotPoint{X: bin[0].x, Label: bin[0].label, Y: reduce(agg, raw)})
	}
	return pts, per
}
