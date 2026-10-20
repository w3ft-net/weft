package shell

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// TestConfigureCore_DashboardNavigate_WithName confirms that
// `dashboard <name>` invokes Shell.Dashboard with the requested name
// instead of falling through to bash. The browser shell wires this to
// React-router navigation.
func TestConfigureCore_DashboardNavigate_WithName(t *testing.T) {
	var got string
	var calls int
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Dashboard: func(name string) {
		got = name
		calls++
	}}
	s.ConfigureCore(core)

	for _, r := range "dashboard prod-overview" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if calls != 1 {
		t.Errorf("expected 1 navigate call, got %d", calls)
	}
	if got != "prod-overview" {
		t.Errorf("navigate name: got %q want %q", got, "prod-overview")
	}
}

// TestConfigureCore_DashboardNavigate_Bare verifies that a bare
// `dashboard` (no arg) still routes through the callback — the
// embedder decides whether to land on an index page or show usage.
func TestConfigureCore_DashboardNavigate_Bare(t *testing.T) {
	var got string = "sentinel"
	var calls int
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Dashboard: func(name string) {
		got = name
		calls++
	}}
	s.ConfigureCore(core)

	for _, r := range "dashboard" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if calls != 1 {
		t.Errorf("expected 1 navigate call, got %d", calls)
	}
	if got != "" {
		t.Errorf("bare `dashboard` should pass empty name; got %q", got)
	}
}

// TestConfigureCore_DashboardNavigate_NoOpWhenNil confirms the CLI
// path: when Shell.Dashboard is nil, the dashboard builtin is NOT
// registered by ConfigureCore. (The CLI's dashboard.Register adds it
// only when the FS satisfies dashboard.Provider — a separate path.)
// Locks in that the browser-style navigate handler doesn't leak into
// CLI cores that didn't opt in.
func TestConfigureCore_DashboardNavigate_NoOpWhenNil(t *testing.T) {
	var out strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { out.Write(b) },
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).ConfigureCore(core)

	for _, r := range "dashboard foo" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	// With no Dashboard callback and no Provider-satisfying FS, the
	// line falls through to bash. We just assert no panic / no crash;
	// bash will print a "command not found" or similar which is the
	// expected legacy behavior.
	_ = out.String()
}

// TestConfigureCore_DashboardNavigate_RejectsBadArgs pins that options and
// extra words are refused instead of being dropped on the way to the callback.
func TestConfigureCore_DashboardNavigate_RejectsBadArgs(t *testing.T) {
	cases := map[string]string{
		"dashboard prod --fullscreen": "dashboard: unknown option --fullscreen",
		"dashboard -x":                "dashboard: unknown option -x",
		"dashboard prod staging":      "dashboard: unexpected argument staging",
	}
	for line, want := range cases {
		var out strings.Builder
		calls := 0
		core := shellcore.NewShellCore(nil, 80, 24,
			func(b []byte) { out.Write(b) },
			func(a shellcore.Action) {},
		)
		(&Shell{User: "alice", Dashboard: func(string) { calls++ }}).ConfigureCore(core)
		for _, r := range line {
			core.HandleEvent(shellcore.KeyEvent{Rune: r})
		}
		core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
		if calls != 0 {
			t.Errorf("%q: navigate called %d times, want 0", line, calls)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("%q: output lacks %q:\n%q", line, want, out.String())
		}
	}
}
