package coordinator

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

// is_absent means "no result": a cron that doesn't exist, one that has never
// run, and one whose results are all older than the window are all absent,
// and every form agrees. The windowed form used to treat a missing cron as
// "can't tell" (exit 1, no fire) while the plain form fired, so
// `weft::is_absent canary-ping` and `weft::is_absent canary-ping 90s`
// disagreed about the same typo'd or deleted cron.
func TestWeftFuncs_IsAbsent_OneRuleForEveryForm(t *testing.T) {
	env := newWFEnv(t)
	env.seedWatch(t, "fresh", []string{"1"}, time.Minute)
	env.saveWatch(t, "never-ran")
	// The stale cron's only result is older than any short window.
	stale := env.saveWatch(t, "stale")
	env.saveResult(t, cron.Result{WatchID: stale.ID, RanAt: time.Now().Add(-48 * time.Hour), Stdout: "1"})
	wf := env.wf()
	pred := predicateAbsent(wf)

	cases := []struct {
		name   string
		cron   string
		window string // "" = the plain form
		absent bool
	}{
		{"missing cron", "ops/no-such-cron", "", true},
		{"missing cron, windowed", "ops/no-such-cron", "90s", true},
		{"never ran", "ops/never-ran", "", true},
		{"never ran, windowed", "ops/never-ran", "90s", true},
		{"has a recent result", "ops/fresh", "", false},
		{"has a recent result, windowed", "ops/fresh", "10m", false},
		{"only an old result, windowed", "ops/stale", "10m", true},
	}
	for _, c := range cases {
		args := []string{c.cron}
		if c.window != "" {
			args = append(args, c.window)
		}
		wantExit := 1
		if c.absent {
			wantExit = 0
		}
		if got := pred(args, nil, io.Discard, io.Discard); got != wantExit {
			t.Errorf("%s: weft::is_absent %v exit %d, want %d", c.name, args, got, wantExit)
		}
		var dur time.Duration
		if c.window != "" {
			dur, _ = time.ParseDuration(c.window)
		}
		got, err := wf.Absent(c.cron, dur)
		if err != nil || got != c.absent {
			t.Errorf("%s: Absent = %v, %v; want %v, nil", c.name, got, err, c.absent)
		}
	}

	// The predicate doesn't fire on an over-cap window, and says why.
	var stderr strings.Builder
	if got := pred([]string{"ops/fresh", "2w"}, nil, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), "exceeds maximum") {
		t.Errorf("is_absent fresh 2w: exit %d, stderr %q; want exit 1 and the cap error", got, stderr.String())
	}

	// A window the history can't answer (over the cap) is "can't tell":
	// not absent, and an error — it must not page for a bad argument.
	if absent, err := wf.Absent("ops/fresh", 365*24*time.Hour); err == nil || absent {
		t.Errorf("over-cap window: Absent = %v, %v; want false and an error", absent, err)
	}
}
