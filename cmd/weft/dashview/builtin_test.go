package dashview

import (
	"errors"
	dspec "github.com/w3ft-net/weft/dashboard"
	"github.com/w3ft-net/weft/testhost"
	"strings"
	"testing"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/chart"
)

func TestParseBuiltinArgs(t *testing.T) {
	for _, c := range []struct {
		args     []string
		name, er string
	}{
		{nil, "", ""},
		{[]string{"prod"}, "prod", ""},
		{[]string{"prod", "--fullscreen"}, "", "unknown option --fullscreen"},
		{[]string{"-x"}, "", "unknown option -x"},
		{[]string{"prod", "staging"}, "", "unexpected argument staging"},
	} {
		name, er := parseBuiltinArgs(c.args)
		if name != c.name || er != c.er {
			t.Errorf("parseBuiltinArgs(%v) = %q, %q; want %q, %q", c.args, name, er, c.name, c.er)
		}
	}
}

type fakeProvider struct {
	names []string
	err   error
}

func (f fakeProvider) GetCronSeries(string, string) (chart.CronSeries, error) {
	return chart.CronSeries{}, nil
}
func (f fakeProvider) GetLatestRun(string) ([]chart.LatestPoint, error) { return nil, nil }
func (f fakeProvider) GetDashboard(string) (string, error)              { return "", nil }
func (f fakeProvider) GetDashboardSpec(string) (*dspec.Spec, error) {
	return &dspec.Spec{Sections: []dspec.Section{{Tiles: []dspec.Tile{{Kind: "sparkline", Watch: "errors"}}}}}, f.err
}
func (f fakeProvider) ListDashboards() ([]string, error) { return f.names, nil }

// cmdCore registers the dashboard command the way the CLI does, in sync or
// async (SetPost) form.
func cmdCore(t *testing.T, p Provider, async bool) (*shellcore.Shell, func(string), func() string) {
	t.Helper()
	var core *shellcore.Shell
	var q chan func()
	if async {
		q = make(chan func(), 256)
		core = testhost.NewAsync(100, 30, func([]byte) {}, func(shellcore.Action) {}, func(fn func()) { q <- fn })
	} else {
		core = testhost.New(100, 30, func([]byte) {}, func(shellcore.Action) {})
	}
	Register(core, p)
	run := func(line string) {
		for _, r := range line {
			core.HandleEvent(shellcore.KeyEvent{Rune: r})
		}
		core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
		if async {
			deadline := time.After(5 * time.Second)
			for core.Mode() == shellcore.ModeRunning {
				select {
				case fn := <-q:
					fn()
				case <-deadline:
					t.Fatal("run did not finish")
				}
			}
		}
	}
	return core, run, func() string { return strings.ReplaceAll(shellcore.StripANSI(core.Scrollback()), "\r\n", "\n") }
}

func eachMode(t *testing.T, fn func(t *testing.T, async bool)) {
	t.Run("sync", func(t *testing.T) { fn(t, false) })
	t.Run("async", func(t *testing.T) { fn(t, true) })
}

func TestCommand_ListsDashboards(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		_, run, out := cmdCore(t, fakeProvider{names: []string{"prod", "staging"}}, async)
		run("dashboard")
		if !strings.Contains(out(), "dashboards:\n  prod\n  staging\n") {
			t.Errorf("out = %q", out())
		}
	})
}

// The dashboard View is entered on the event loop and outlives the run.
func TestCommand_EntersTheViewAndKeepsIt(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		core, run, _ := cmdCore(t, fakeProvider{}, async)
		run("dashboard prod")
		if core.Mode() != shellcore.ModeView {
			t.Errorf("mode = %v, want ModeView", core.Mode())
		}
	})
}

func TestCommand_FailuresExitNonZero(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		for line, want := range map[string]string{
			"dashboard -x":  "unknown option -x",
			"dashboard a b": "unexpected argument b",
		} {
			core, run, out := cmdCore(t, fakeProvider{}, async)
			done := shellcore.Subscribe[shellcore.CommandCompleted](core)
			run(line)
			if !strings.Contains(out(), want) {
				t.Errorf("%q: out = %q", line, out())
			}
			if ev := <-done; ev.ExitCode == 0 {
				t.Errorf("%q exited 0", line)
			}
		}
		core, run, out := cmdCore(t, fakeProvider{err: errors.New("no such dashboard")}, async)
		done := shellcore.Subscribe[shellcore.CommandCompleted](core)
		run("dashboard nope")
		if !strings.Contains(out(), "no such dashboard") || (<-done).ExitCode == 0 {
			t.Errorf("a missing dashboard should fail visibly: %q", out())
		}
	})
}
