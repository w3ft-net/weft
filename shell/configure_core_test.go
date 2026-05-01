package shell

import (
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
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
// configureCore (where the embedder owns its own naming).
func TestConfigureCore_DefaultsToWeftCustomerWhenUnset(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)

	(&Shell{User: "alice"}).configureCore(core)
	core.Redraw()

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "alice@weft") {
		t.Errorf("prompt should contain alice@weft when Customer is unset; got: %q", visible)
	}
}

// TestConfigureCore_RangeBuiltinDisplaysCurrentWindow verifies that
// `range` (no arg) prints the current time window. Locks in the
// behavior moved out of warp's inline shellcore dispatch — the
// builtin is now embedder-registered via configureCore, and bare
// invocation must still produce a "range: <start> .. <end>" line.
func TestConfigureCore_RangeBuiltinDisplaysCurrentWindow(t *testing.T) {
	var output strings.Builder
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { output.Write(b) },
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).configureCore(core)

	for _, r := range "range" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "range:") {
		t.Errorf("bare `range` should display current window header; got: %q", visible)
	}
}

// TestConfigureCore_RangeBuiltinAcceptsSpec verifies that
// `range 30m..` parses + dispatches an ActionSetRange. The builtin
// must update both the core's internal range AND fire the action
// the embedder uses to retransmit the window upstream.
func TestConfigureCore_RangeBuiltinAcceptsSpec(t *testing.T) {
	var dispatched bool
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {
			if _, ok := a.(shellcore.ActionSetRange); ok {
				dispatched = true
			}
		},
	)
	(&Shell{User: "alice"}).configureCore(core)

	for _, r := range "range 30m.." {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if !dispatched {
		t.Error("range <spec> should dispatch ActionSetRange")
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

	(&Shell{User: "alice", Customer: "acme"}).configureCore(core)
	core.Redraw()

	visible := stripANSI(output.String())
	if !strings.Contains(visible, "alice@acme") {
		t.Errorf("explicit Customer should win; got: %q", visible)
	}
	if strings.Contains(visible, "alice@weft") {
		t.Errorf("explicit Customer should suppress @weft fallback; got: %q", visible)
	}
}
