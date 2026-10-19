package shell

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// stripANSI strips ANSI escape sequences for assertions on visible
// prompt content. Trimmed copy of the helper in warp's core_test.go.
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestConfigureCore_DefaultsToWeftCustomerWhenUnset locks in the
// user-visible contract: a Shell that hasn't set Customer renders
// the prompt with `@weft` so weft's default tenancy is reflected
// without the embedder having to set it on every construction.
//
// This test is the regression guard around the cleanup that moves
// the "weft" default out of warp's shellcore (where it was a
// hardcoded fallback for the customer field) into weft's
// ConfigureCore (where the embedder owns its own naming).
func TestConfigureCore_DefaultsToWeftCustomerWhenUnset(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)

	(&Shell{User: "alice"}).ConfigureCore(core)
	core.Redraw()

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "alice@weft") {
		t.Errorf("prompt should contain alice@weft when Customer is unset; got: %q", visible)
	}
}

// TestConfigureCore_RangeBuiltinDisplaysCurrentWindow verifies that
// `range` (no arg) prints the current time window. Locks in the
// behavior moved out of warp's inline shellcore dispatch — the
// builtin is now embedder-registered via ConfigureCore, and bare
// invocation must still produce a "range: <start> .. <end>" line.
func TestConfigureCore_RangeBuiltinDisplaysCurrentWindow(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).ConfigureCore(core)

	for _, r := range "range" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "range:") {
		t.Errorf("bare `range` should display current window header; got: %q", visible)
	}
}

// TestConfigureCore_RangeBuiltinAcceptsSpec verifies that `range 30m..`
// parses, stores the un-resolved spec on the Shell, and fires the
// out-of-band OnRangeChange hook embedders use to retransmit the window
// upstream. warp removed shellcore's internal range + ActionSetRange;
// the spec now lives on the Shell and the prompt segment is set via
// SetLeadSegment, so the hook (not a dispatched action) is the contract.
func TestConfigureCore_RangeBuiltinAcceptsSpec(t *testing.T) {
	var got string
	s := &Shell{User: "alice", OnRangeChange: func(spec string) { got = spec }}
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	s.ConfigureCore(core)

	for _, r := range "range 30m.." {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if got != "30m.." {
		t.Errorf("range <spec> should fire OnRangeChange(%q); got %q", "30m..", got)
	}
	if s.Range != "30m.." {
		t.Errorf("range <spec> should store the un-resolved spec on s.Range; got %q", s.Range)
	}
}

// TestConfigureCore_HelpBuiltinsServesWeftCorpus locks in the
// behavior that `help builtins` prints the weft-flavored corpus
// (cat /log/<app>.log, ls /cron.d, git, alert, range, etc.). Test
// is the regression guard around the move that takes the docs_gen
// constants + dispatch out of warp's shellcore (where it was the
// hardcoded default) and into weft (where it lives as the
// HelpProvider weft installs via ConfigureCore).
//
// Pre-move: passes via warp's default shellDocTopics map.
// Post-move: passes via weft's HelpProvider returning the same
// corpus. Same observable result either way.
func TestConfigureCore_HelpBuiltinsServesWeftCorpus(t *testing.T) {
	var output strings.Builder
	// Use a tall viewport so the help corpus renders inline rather
	// than entering the pager (which only shows the first screenful
	// in scrollback) — tests assert on the full corpus.
	core := shellcore.NewShellCore(nil, 200, 200,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).ConfigureCore(core)

	for _, r := range "help builtins" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	visible := stripANSI(output.String())
	for _, marker := range []string{
		"Reading logs",
		"/log/",
		"Shell extensions",
		"git",
		"chart",
	} {
		if !strings.Contains(visible, marker) {
			t.Errorf("help builtins should mention %q; got: %q", marker, visible)
		}
	}
}

// TestConfigureCore_HelpCronInterpolatesTierLimits exercises the
// {{TIER_LIMITS}} placeholder in the cron help topic. The provider
// must consult weft's storeTierLimits to render the per-tier watch
// caps. Pre-move warp held this logic; after the cleanup it lives
// in weft's HelpProvider.
func TestConfigureCore_HelpCronInterpolatesTierLimits(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 200, 200,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).ConfigureCore(core)

	for _, r := range "help cron" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	visible := stripANSI(output.String())
	if strings.Contains(visible, "{{TIER_LIMITS}}") {
		t.Error("help cron should interpolate {{TIER_LIMITS}} placeholder")
	}
}

// TestConfigureCore_HonoursExplicitCustomer verifies the override
// path: when Shell.Customer is set, that wins over the "weft"
// fallback.
func TestConfigureCore_HonoursExplicitCustomer(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)

	(&Shell{User: "alice", Customer: "acme"}).ConfigureCore(core)
	core.Redraw()

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "alice@acme") {
		t.Errorf("explicit Customer should win; got: %q", visible)
	}
	if strings.Contains(visible, "alice@weft") {
		t.Errorf("explicit Customer should suppress @weft fallback; got: %q", visible)
	}
}

// typeLine feeds a line to the core as keystrokes and returns what it
// printed, without ANSI.
func typeLine(t *testing.T, line string) string {
	t.Helper()
	var output strings.Builder
	var actions []shellcore.Action
	core := shellcore.NewShellCore(nil, 200, 200,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) { actions = append(actions, a) },
	)
	(&Shell{User: "alice"}).ConfigureCore(core)
	for _, r := range line {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
	if len(actions) > 0 {
		t.Errorf("%q was sent to the server (%v) instead of answered by help", line, actions)
	}
	return stripANSI(output.String())
}

// `man explain` and `explain --help` show the man page, as `man range` does.
func TestConfigureCore_ManExplain(t *testing.T) {
	for _, line := range []string{"man explain", "explain --help"} {
		if got := typeLine(t, line); !strings.Contains(got, "EXPLAIN(1)") {
			t.Errorf("%q: no man page in %q", line, got)
		}
	}
}

// `man commands` is the page about weft's own commands, and every command
// it points to has a page of its own.
func TestConfigureCore_ManCommandsPointsAtRealPages(t *testing.T) {
	page := typeLine(t, "man commands")
	if !strings.Contains(page, "COMMANDS(1)") {
		t.Fatalf("no commands page in %q", page)
	}
	for _, topic := range []string{"range", "explain", "chart", "less", "alert", "notify", "weft-funcs", "dashboard", "git", "clipboard", "sharing", "passwd", "teams"} {
		if !strings.Contains(page, "man "+topic) {
			t.Errorf("commands page doesn't mention man %s", topic)
		}
		if got := typeLine(t, "man "+topic); strings.TrimSpace(got) == "" || strings.Contains(got, "No manual entry") {
			t.Errorf("man %s shows nothing useful: %q", topic, got)
		}
	}
	if !strings.Contains(typeLine(t, "man"), "man commands") {
		t.Error("bare man doesn't list commands")
	}
}
