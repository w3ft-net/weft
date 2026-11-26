package shell

import (
	"strings"
	"sync"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// rangeCore is a remote core whose terminal output is captured, so the prompt
// the user sees (and its [window] segment) can be checked.
func rangeCore(t *testing.T, s *Shell, async bool) (*shellcore.Shell, *strings.Builder, chan func()) {
	t.Helper()
	var mu sync.Mutex
	var out strings.Builder
	write := func(b []byte) { mu.Lock(); out.Write(b); mu.Unlock() }
	var q chan func()
	var core *shellcore.Shell
	if async {
		q = make(chan func(), 1024)
		core = newCoreAsync(80, 24, write, func(shellcore.Action) {}, func(fn func()) { q <- fn })
	} else {
		core = newCoreD(80, 24, write, func(shellcore.Action) {})
	}
	if s.FS == nil {
		s.FS = testMountFS(nil)
	}
	s.ConfigureCore(core)
	return core, &out, q
}

func settle(t *testing.T, core *shellcore.Shell, q chan func()) {
	t.Helper()
	if q != nil {
		pump(t, q, func() bool { return core.Mode() == shellcore.ModeInput })
	}
}

func TestRangeCommand_SetsTheWindowEverywhere(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := map[bool]string{false: "sync", true: "async"}[async]
		t.Run(name, func(t *testing.T) {
			rec := &recorder{result: &QueryResult{Text: "x\n"}}
			var changed []string
			s := &Shell{Query: rec.query, Range: "1d..", OnRangeChange: func(spec string) { changed = append(changed, spec) }}
			core, out, q := rangeCore(t, s, async)

			enter(core, "range 30m..")
			settle(t, core, q)

			if s.Range != "30m.." {
				t.Errorf("s.Range = %q", s.Range)
			}
			if len(changed) != 1 || changed[0] != "30m.." {
				t.Errorf("OnRangeChange calls = %v", changed)
			}
			if !strings.Contains(shellcore.StripANSI(out.String()), "[30m]") {
				t.Errorf("the prompt should show the new window; output %q", shellcore.StripANSI(out.String()))
			}
			if len(rec.lines) != 0 {
				t.Errorf("range must not be shipped to the server: %q", rec.lines)
			}

			// The next query carries the new window.
			enter(core, "cat x")
			settle(t, core, q)
			if len(rec.ranges) != 1 || rec.ranges[0] != "30m.." {
				t.Errorf("next query shipped range %v, want 30m..", rec.ranges)
			}
		})
	}
}

func TestRangeCommand_BareShowsTheCurrentWindowWithoutChangingIt(t *testing.T) {
	rec := &recorder{result: &QueryResult{}}
	s := &Shell{Query: rec.query, Range: "1h.."}
	core, _, _ := rangeCore(t, s, false)
	enter(core, "range")
	if !strings.Contains(screen(core), "1h") || s.Range != "1h.." {
		t.Errorf("screen %q, s.Range %q", screen(core), s.Range)
	}
}

func TestRangeCommand_BadSpecFailsAndKeepsTheWindow(t *testing.T) {
	rec := &recorder{result: &QueryResult{}}
	s := &Shell{Query: rec.query, Range: "1d.."}
	core, _, _ := rangeCore(t, s, false)
	done := shellcore.Subscribe[shellcore.CommandCompleted](core)
	enter(core, "range not-a-range")
	if s.Range != "1d.." {
		t.Errorf("a bad spec changed s.Range to %q", s.Range)
	}
	if !strings.Contains(screen(core), "range:") {
		t.Errorf("no error shown: %q", screen(core))
	}
	if ev := <-done; ev.ExitCode == 0 {
		t.Error("a bad range exited 0")
	}
}

// Help topics are keyed by the whole line ("range --help"), so they must win
// over a client command of the same name: registering the command must not hide
// its manual page.
func TestHelpTopicsBeatClientCommands(t *testing.T) {
	for line, want := range map[string]string{
		"range --help":     "RANGE(1)",
		"chart --help":     "CHART(1)",
		"dashboard --help": "DASHBOARD(1)",
		"trace --help":     "TRACE(1)",
	} {
		rec := &recorder{result: &QueryResult{Text: "shipped\n"}}
		s := &Shell{
			Query: rec.query, Range: "1d..",
			Chart: func(ChartRequest) {}, Dashboard: func(string) {}, Trace: func(TraceRequest) {},
		}
		core, out, _ := rangeCore(t, s, false)
		enter(core, line)
		// A manual page is long enough to open in the pager, so read what was
		// written to the terminal rather than scrollback.
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q: expected the manual page, got %q", line, shellcore.StripANSI(out.String()))
		}
		if len(rec.lines) != 0 {
			t.Errorf("%q was shipped to the server: %q", line, rec.lines)
		}
	}
}
