package chart

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

type fakeCronSrc struct {
	series CronSeries
	err    error
}

func (f fakeCronSrc) GetCronSeries(string, string) (CronSeries, error) { return f.series, f.err }

// cmdCore builds a core with chart registered the way the CLI does, in sync or
// async (SetPost) form, and returns helpers to type a line and settle the run.
func cmdCore(t *testing.T, src CronSource, async bool) (*shellcore.ShellCore, func(string), func() string) {
	t.Helper()
	core := shellcore.NewShellCore(nil, 100, 30, func([]byte) {}, func(shellcore.Action) {})
	Register(core, src, func() string { return "2h.." })
	var q chan func()
	if async {
		q = make(chan func(), 256)
		core.SetPost(func(fn func()) { q <- fn })
	}
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
	out := func() string { return strings.ReplaceAll(shellcore.StripANSI(core.Scrollback()), "\r\n", "\n") }
	return core, run, out
}

func eachMode(t *testing.T, fn func(t *testing.T, async bool)) {
	t.Run("sync", func(t *testing.T) { fn(t, false) })
	t.Run("async", func(t *testing.T) { fn(t, true) })
}

func rawBuckets() CronSeries {
	return CronSeries{Granularity: "raw", Buckets: []CronBucket{raw(0, "", 1), raw(5, "", 3), raw(10, "", 2), raw(15, "", 5)}}
}

func TestCommand_WatchFormRendersTheCron(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		_, run, out := cmdCore(t, fakeCronSrc{series: rawBuckets()}, async)
		run("chart errors --title peak")
		if !strings.Contains(out(), "peak") {
			t.Errorf("no chart on screen: %q", out())
		}
	})
}

func TestCommand_PipeFormRendersTheUpstreamLines(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		_, run, out := cmdCore(t, fakeCronSrc{}, async)
		run(`printf '200 a\n201 b\n199 c\n' | chart --kind line --title latency`)
		if !strings.Contains(out(), "latency") {
			t.Errorf("no chart on screen: %q", out())
		}
	})
}

func TestCommand_FailureExitsNonZero(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		core, run, out := cmdCore(t, fakeCronSrc{err: errors.New("no such cron")}, async)
		done := shellcore.Subscribe[shellcore.CommandCompleted](core)
		run("chart missing")
		if !strings.Contains(out(), "no such cron") {
			t.Errorf("error not shown: %q", out())
		}
		if ev := <-done; ev.ExitCode == 0 {
			t.Error("a failed chart exited 0")
		}
	})
}

// --follow hands the screen to the chart View, and the run ending must not
// take it back.
func TestCommand_FollowEntersTheViewAndKeepsIt(t *testing.T) {
	eachMode(t, func(t *testing.T, async bool) {
		core, run, _ := cmdCore(t, fakeCronSrc{series: rawBuckets()}, async)
		run("chart errors --follow")
		if core.Mode() != shellcore.ModeView {
			t.Errorf("mode = %v, want ModeView", core.Mode())
		}
	})
}
