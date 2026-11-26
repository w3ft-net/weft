package dashview

import (
	weftchart "github.com/w3ft-net/weft/chart"
	dspec "github.com/w3ft-net/weft/dashboard"
)

// resultKey names the data a tile draws: its cron's stored series over the
// tile's range. Tiles on one cron share an entry only when they share a
// range; each tile then reduces it with its own aggregate and bucket.
func resultKey(t dspec.Tile) string { return t.Watch + "|" + t.TileRange() }

// fetchAllSeries pulls the data for every distinct (cron, range) the spec
// names: the stored series over the range, and the cron's latest run for
// counter, gauge and table tiles. Missing crons end up absent from the map;
// the renderer treats those as <no data>.
func fetchAllSeries(p Provider, spec *dspec.Spec) map[string]WatchSeries {
	out := map[string]WatchSeries{}
	latest := map[string][]weftchart.LatestPoint{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			key := resultKey(tile)
			if _, seen := out[key]; seen {
				continue
			}
			series, err := p.GetCronSeries(tile.Watch, tile.TileRange())
			if err != nil {
				continue
			}
			if _, ok := latest[tile.Watch]; !ok {
				latest[tile.Watch], _ = p.GetLatestRun(tile.Watch)
			}
			out[key] = WatchSeries{Series: series, Latest: latest[tile.Watch]}
		}
	}
	return out
}
