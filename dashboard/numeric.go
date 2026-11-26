package dashboard

import "sort"

// numericTileKinds are the tiles that draw numbers; a table shows text as well.
var numericTileKinds = map[string]bool{"line": true, "bar": true, "sparkline": true, "counter": true, "gauge": true}

// ReadsNumbers reports whether t draws numbers: a table does only when it
// aggregates over a range, which text can't be.
func ReadsNumbers(t Tile) bool {
	return numericTileKinds[t.Kind] || (t.Kind == "table" && t.Windowed())
}

// NumericTiles returns the kinds, sorted, of spec's numeric tiles on the cron
// named cron (<namespace>/<name>).
func NumericTiles(spec *Spec, cron string) []string {
	seen := map[string]bool{}
	for _, sec := range spec.Sections {
		for _, t := range sec.Tiles {
			if t.Watch == cron && ReadsNumbers(t) {
				seen[t.Kind] = true
			}
		}
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}
