package dashboard

import (
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
)

// Provider is the data source the dashboard builtin needs:
//   - GetDashboard fetches the YAML body for a named dashboard.
//   - GetDashboardSpec returns the cached parsed spec (avoids re-fetch + YAML parse).
//   - ListDashboards returns available dashboard names.
//   - GetWatchResults returns a cron's recent runs (the latest feeds
//     counter, gauge and table tiles).
//   - GetCronSeries (chart.CronSource) returns a cron's stored points
//     per tag, for sparkline tiles.
type Provider interface {
	chart.CronSource
	GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error)
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
