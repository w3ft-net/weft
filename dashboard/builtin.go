package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
)

// Provider is the data source the dashboard builtin needs:
//   - GetDashboard fetches the YAML body for a named dashboard.
//   - GetDashboardSpec returns the cached parsed spec (avoids re-fetch + YAML parse).
//   - ListDashboards returns available dashboard names.
//   - GetWatchResults is the same hook chart.WatchResultsProvider uses
//     so existing FS implementations satisfy it for free.
type Provider interface {
	chart.WatchResultsProvider
	GetDashboard(name string) (string, error)
	GetDashboardSpec(name string) (*store.DashboardSpec, error)
	ListDashboards() ([]string, error)
}

// Builtin returns a shellcore.BuiltinFunc that handles `dashboard <name>`
// typed at the prompt. Renders the frame immediately from the cached
// spec, then fills tile data asynchronously.
//
// Wire it via core.RegisterBuiltin("dashboard", dashboard.Builtin(p)).
func Builtin(p Provider) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		if len(parts) == 0 || parts[0] != "dashboard" {
			return false
		}
		if len(parts) < 2 {
			names, err := p.ListDashboards()
			if err != nil || len(names) == 0 {
				c.AppendScrollback(shellcore.DimText("(no dashboards)") + "\n")
				return true
			}
			c.AppendScrollback("dashboards:\n")
			for _, n := range names {
				c.AppendScrollback("  " + n + "\n")
			}
			return true
		}
		name := parts[1]
		spec, err := p.GetDashboardSpec(name)
		if err != nil {
			c.AppendScrollback(shellcore.ErrorText("dashboard: "+err.Error()) + "\n")
			return true
		}
		if err := c.EnterView(viewName, &ViewArgs{Name: name, Spec: spec}); err != nil {
			c.AppendScrollback(shellcore.ErrorText("dashboard: "+err.Error()) + "\n")
		}
		return true
	}
}

// Register wires the dashboard builtin and view into the given shellcore.
func Register(c *shellcore.ShellCore, p Provider) {
	c.RegisterView(NewView(p))
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
		t, err := time.Parse(time.RFC3339, r.RanAt)
		if err != nil {
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
		ws.Times = append(ws.Times, float64(t.Unix()))
	}
	return ws
}
