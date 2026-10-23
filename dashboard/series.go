package dashboard

import (
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
)

// seriesRange is the window a terminal dashboard draws, the same day the
// web dashboard shows.
const seriesRange = "24h.."

// fetchAllSeries pulls per-cron data for every distinct cron named in the
// spec: the stored series over seriesRange, reduced like `chart <cron>`
// (one line per tag, the rest folded into "other"), and the latest run's
// points for table tiles. Missing crons end up absent from the map; the
// renderer treats those as <no data>.
func fetchAllSeries(p Provider, spec *store.DashboardSpec) map[string]WatchSeries {
	out := map[string]WatchSeries{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if _, seen := out[tile.Watch]; seen {
				continue
			}
			series, err := p.GetCronSeries(tile.Watch, seriesRange)
			if err != nil {
				continue
			}
			ws, _ := toWatchSeries(series)
			if latest, err := p.GetLatestRun(tile.Watch); err == nil {
				ws.Latest = latest
			}
			out[tile.Watch] = ws
		}
	}
	return out
}

// toWatchSeries reduces a cron's stored series to what the renderer draws:
// Values/Times (newest-first) for an untagged cron, one entry per tag in
// Tags otherwise.
func toWatchSeries(series chart.CronSeries) (WatchSeries, string) {
	plot, msg := chart.CronPlot(series, chart.WatchArgs{Kind: "line", Aggregate: "avg"})
	if msg != "" {
		return WatchSeries{}, msg
	}
	flat := func(s chart.PlotSeries) WatchSeries {
		ws := WatchSeries{}
		for i := len(s.Points) - 1; i >= 0; i-- {
			ws.Values = append(ws.Values, s.Points[i].Y)
			ws.Times = append(ws.Times, s.Points[i].X)
		}
		return ws
	}
	switch {
	case len(plot.Series) == 0:
		return WatchSeries{}, ""
	case len(plot.Series) == 1 && plot.Series[0].Tag == "":
		return flat(plot.Series[0]), ""
	}
	ws := WatchSeries{Tags: make(map[string]WatchSeries, len(plot.Series))}
	for _, s := range plot.Series {
		ws.Tags[s.Tag] = flat(s)
	}
	return ws, ""
}
