package spanview

import (
	"github.com/w3ft-net/weft/testhost"
	"strings"
	"testing"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

func cmdCore(t *testing.T, async bool) (*shellcore.Shell, func(string)) {
	t.Helper()
	var core *shellcore.Shell
	var q chan func()
	if async {
		q = make(chan func(), 256)
		core = testhost.NewAsync(100, 30, func([]byte) {}, func(shellcore.Action) {}, func(fn func()) { q <- fn })
	} else {
		core = testhost.New(100, 30, func([]byte) {}, func(shellcore.Action) {})
	}
	Register(core, func(string) ([]string, error) { return nil, nil })
	return core, func(line string) {
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
}

// The span views are entered as a View that outlives the run.
func TestCommand_EntersTheSpanViewAndKeepsIt(t *testing.T) {
	for _, async := range []bool{false, true} {
		core, run := cmdCore(t, async)
		run("trace --spans --id abc-123")
		if core.Mode() != shellcore.ModeView {
			t.Errorf("async=%v: mode = %v, want ModeView", async, core.Mode())
		}
	}
}

// The shapes the View does not handle are yielded by the embedder's remote
// hook through Claims; run directly, the command refuses them.
func TestCommand_RefusesWhatItDoesNotHandle(t *testing.T) {
	core, run := cmdCore(t, false)
	done := shellcore.Subscribe[shellcore.CommandCompleted](core)
	run("trace --id abc-123")
	if ev := <-done; ev.ExitCode == 0 {
		t.Error("an unhandled shape exited 0")
	}
	if core.Mode() == shellcore.ModeView {
		t.Error("an unhandled shape opened the view")
	}
	if !strings.Contains(strings.ToLower(shellcore.StripANSI(core.Scrollback())), "trace") {
		t.Errorf("no error shown: %q", core.Scrollback())
	}
}

func TestClaims(t *testing.T) {
	for args, want := range map[string]bool{
		"--spans --id abc":           true,
		"--spans":                    true,
		"--spans acme/api --name x":  true,
		"--id abc":                   false,
		"":                           false,
		"--spans --name x --samples": false,
		"--bogus":                    false,
	} {
		var a []string
		if args != "" {
			a = strings.Fields(args)
		}
		if got := Claims(a); got != want {
			t.Errorf("Claims(%q) = %v, want %v", args, got, want)
		}
	}
}
