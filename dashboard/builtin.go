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
		name, errMsg := parseBuiltinArgs(parts[1:])
		if errMsg != "" {
			c.AppendScrollback(shellcore.ErrorText("dashboard: "+errMsg) + "\n")
			return true
		}
		if name == "" {
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

// parseBuiltinArgs takes the words after `dashboard`: at most one name, and
// no options.
func parseBuiltinArgs(args []string) (name, errMsg string) {
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			return "", "unknown option " + a
		case name != "":
			return "", "unexpected argument " + a
		default:
			name = a
		}
	}
	return name, ""
}

// Register wires the dashboard builtin and view into the given shellcore.
func Register(c *shellcore.ShellCore, p Provider) {
	c.RegisterView(NewView(p))
	c.RegisterBuiltin("dashboard", Builtin(p))
}

// fetchAllSeries pulls per-watch series for every distinct watch named
// in the spec. Missing watches end up absent from the map; the
// renderer treats those as <no data>.
//
// A tagged tile (tile.Tag set) and an untagged tile can't share the
// same watch's cached WatchSeries — one wants Values/Times, the other
// wants Tags — but the dashboard-builder grammar already restricts
// tag:/y: to sparkline tiles and requires them together, so in
// practice a given watch is either always-tagged or never-tagged
// across a spec. Keyed by watch name like the untagged path, using
// whichever tile referencing it is seen first.
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
			if tile.Tag != "" {
				out[tile.Watch] = toTaggedWatchSeries(results, tile.Tag, tile.Y)
			} else {
				out[tile.Watch] = toWatchSeries(results)
			}
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

// toTaggedWatchSeries is toWatchSeries' --tag counterpart: instead of
// one scalar per run (the last whitespace field of the whole stdout),
// a tagged watch's stdout is one JSON row per tagged entity per run
// (e.g. one row per coordinator instance). Every row is walked
// independently and assigned to its tag's own WatchSeries, sharing
// that run's RanAt as the x value for every row in it — a dashboard
// tile's history comes from cron run history, not a live pipeline, so
// there's no per-row timestamp to bucket against the way chart.PipeArgs
// does for `... | chart --tag`.
func toTaggedWatchSeries(results []chart.WatchResultValue, tagPath, yPath string) WatchSeries {
	tagSpec, err := shellcore.ParseFieldSpec(tagPath)
	if err != nil {
		return WatchSeries{}
	}
	ySpec, err := shellcore.ParseFieldSpec(yPath)
	if err != nil {
		return WatchSeries{}
	}

	tags := map[string]*WatchSeries{}
	for _, r := range results {
		t, err := time.Parse(time.RFC3339, r.RanAt)
		if err != nil {
			continue
		}
		x := float64(t.Unix())
		for _, line := range strings.Split(r.Value, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			tagVal, err := shellcore.ResolveField(tagSpec, line)
			if err != nil || tagVal == "" {
				continue
			}
			yStr, err := shellcore.ResolveField(ySpec, line)
			if err != nil || yStr == "" {
				continue
			}
			y, err := strconv.ParseFloat(yStr, 64)
			if err != nil {
				continue
			}
			ts, ok := tags[tagVal]
			if !ok {
				ts = &WatchSeries{}
				tags[tagVal] = ts
			}
			ts.Values = append(ts.Values, y)
			ts.Times = append(ts.Times, x)
		}
	}
	if len(tags) == 0 {
		return WatchSeries{}
	}
	out := WatchSeries{Tags: make(map[string]WatchSeries, len(tags))}
	for tag, ts := range tags {
		out.Tags[tag] = *ts
	}
	return out
}
