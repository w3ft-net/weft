package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/chart"
	"github.com/kevlarcon/weft/store"
)

// Provider is the data source the dashboard builtin needs:
//   - GetDashboard fetches the YAML body for a named dashboard.
//   - GetWatchResults is the same hook chart.WatchResultsProvider uses
//     so existing FS implementations satisfy it for free.
type Provider interface {
	chart.WatchResultsProvider
	GetDashboard(name string) (string, error)
}

// Builtin returns a shellcore.BuiltinFunc that handles `dashboard <name>`
// typed at the prompt. Reads the YAML, parses it, fetches each
// referenced watch's results, and writes the rendered terminal layout
// to scrollback.
//
// Wire it via core.RegisterBuiltin("dashboard", dashboard.Builtin(p)).
func Builtin(p Provider) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		if len(parts) == 0 || parts[0] != "dashboard" {
			return false
		}
		if len(parts) < 2 {
			c.AppendScrollback(shellcore.ErrorText("dashboard: name required (try `dashboard <name>`)") + "\n")
			return true
		}
		name := parts[1]
		body, err := p.GetDashboard(name)
		if err != nil {
			c.AppendScrollback(shellcore.ErrorText("dashboard: "+err.Error()) + "\n")
			return true
		}
		spec, err := store.ParseDashboardYAML(body)
		if err != nil {
			c.AppendScrollback(shellcore.ErrorText("dashboard: "+err.Error()) + "\n")
			return true
		}
		results := fetchAllSeries(p, spec)
		w := c.Width()
		if w < 20 {
			w = 20
		}
		out := Render(spec, results, w)
		c.AppendScrollback(out)
		return true
	}
}

// Register wires the dashboard builtin into the given shellcore.
func Register(c *shellcore.ShellCore, p Provider) {
	c.RegisterBuiltin("dashboard", Builtin(p))
}

// fetchAllSeries pulls per-watch series for every distinct watch named
// in the spec. Missing watches end up absent from the map; the
// renderer treats those as <no data>.
func fetchAllSeries(p Provider, spec *store.DashboardSpec) map[string]WatchSeries {
	out := map[string]WatchSeries{}
	for _, sec := range spec.Sections {
		for _, tile := range sec.Tiles {
			if _, seen := out[tile.Watch]; seen {
				continue
			}
			results, err := p.GetWatchResults(tile.Watch, 50)
			if err != nil {
				continue
			}
			out[tile.Watch] = toWatchSeries(results)
		}
	}
	return out
}

// toWatchSeries converts chart.WatchResultValue (newest-first) into a
// WatchSeries the renderer consumes. Values track the parsed numeric
// last-field (matching watchResultsToPoints' convention); LatestText is
// the most recent stdout for table tiles.
func toWatchSeries(results []chart.WatchResultValue) WatchSeries {
	ws := WatchSeries{}
	if len(results) == 0 {
		return ws
	}
	ws.LatestText = strings.TrimSpace(results[0].Value)
	for _, r := range results {
		// Skip rows that don't parse as RFC3339; otherwise they'd show
		// up as the literal text. Parsing is cheap and gates the
		// numeric extraction.
		if _, err := time.Parse(time.RFC3339, r.RanAt); err != nil {
			continue
		}
		val := strings.TrimSpace(r.Value)
		if val == "" {
			continue
		}
		fields := strings.Fields(val)
		if len(fields) == 0 {
			continue
		}
		f, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		ws.Values = append(ws.Values, f)
	}
	return ws
}
