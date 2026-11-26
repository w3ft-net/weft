package dashview

import (
	"fmt"
	"strings"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/chart"
	dspec "github.com/w3ft-net/weft/dashboard"
)

// Provider is the data source the dashboard command needs:
//   - GetDashboard fetches the YAML body for a named dashboard.
//   - GetDashboardSpec returns the cached parsed spec (avoids re-fetch + YAML parse).
//   - ListDashboards returns available dashboard names.
//   - GetLatestRun returns the points of a cron's latest run (for
//     counter, gauge and table tiles).
//   - GetCronSeries (chart.CronSource) returns a cron's stored points
//     per tag, for sparkline tiles.
type Provider interface {
	chart.CronSource
	GetLatestRun(name string) ([]chart.LatestPoint, error)
	GetDashboard(name string) (string, error)
	GetDashboardSpec(name string) (*dspec.Spec, error)
	ListDashboards() ([]string, error)
}

// Command returns the `dashboard` command: `dashboard` lists the available
// dashboards and `dashboard <name>` enters the dashboard View, rendering the
// frame immediately from the cached spec and filling tile data asynchronously.
// The View is entered on the event loop and outlives the run.
//
// Wire it via Register, or core.RegisterCommand("dashboard", dashboard.Command(p))
// with core.RegisterView(dashboard.NewView(p)).
func Command(p Provider) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		fail := func(msg string) int {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText("dashboard: "+msg))
			return 1
		}
		name, errMsg := parseBuiltinArgs(cx.Args)
		if errMsg != "" {
			return fail(errMsg)
		}
		if name == "" {
			names, err := p.ListDashboards()
			if err != nil || len(names) == 0 {
				fmt.Fprintln(cx.Stdout, shellcore.DimText("(no dashboards)"))
				return 0
			}
			fmt.Fprintln(cx.Stdout, "dashboards:")
			for _, n := range names {
				fmt.Fprintln(cx.Stdout, "  "+n)
			}
			return 0
		}
		spec, err := p.GetDashboardSpec(name)
		if err != nil {
			return fail(err.Error())
		}
		if err := cx.OnLoop(func(c *shellcore.Shell) error {
			return c.EnterView(viewName, &ViewArgs{Name: name, Spec: spec})
		}); err != nil {
			return fail(err.Error())
		}
		return 0
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

// Register wires the dashboard command and view into the given shellcore.
func Register(c *shellcore.Shell, p Provider) {
	c.RegisterView(NewView(p))
	c.RegisterCommand("dashboard", Command(p))
}
