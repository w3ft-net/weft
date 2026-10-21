package shell

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

func runTraceLine(t *testing.T, line string) []TraceRequest {
	t.Helper()
	var got []TraceRequest
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Trace: func(r TraceRequest) {
		got = append(got, r)
	}}
	s.ConfigureCore(core)

	for _, r := range line {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
	return got
}

// TestConfigureCore_TraceBuiltin_PlainID verifies that a bare `trace
// <id>` fires Shell.Trace with Mode "gap" — the shape spanview.Builtin
// deliberately doesn't intercept, since the CLI renders it via
// RenderTraceIfApplicable rather than entering a View.
func TestConfigureCore_TraceBuiltin_PlainID(t *testing.T) {
	got := runTraceLine(t, "trace --id abc123")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	r := got[0]
	if r.Mode != "gap" {
		t.Errorf("Mode: got %q want gap", r.Mode)
	}
	if r.Pipeline != "trace --id abc123" {
		t.Errorf("Pipeline: got %q want %q", r.Pipeline, "trace --id abc123")
	}
	if r.Title != "abc123" {
		t.Errorf("Title: got %q want abc123", r.Title)
	}
}

// TestConfigureCore_TraceBuiltin_SpansFlag verifies `trace <id>
// --spans` fires Mode "rollup" (Phase 1) with the flag stripped from
// the re-run pipeline, matching spanview.ParseSpansLine's own shape.
func TestConfigureCore_TraceBuiltin_SpansFlag(t *testing.T) {
	got := runTraceLine(t, "trace --id abc123 --spans")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	r := got[0]
	if r.Mode != "rollup" {
		t.Errorf("Mode: got %q want rollup", r.Mode)
	}
	if r.Pipeline != "trace --spans --id abc123" {
		t.Errorf("Pipeline: got %q want %q", r.Pipeline, "trace --spans --id abc123")
	}
	if r.Title != "abc123" {
		t.Errorf("Title: got %q want abc123", r.Title)
	}
}

// TestConfigureCore_TraceBuiltin_SpansAggregate verifies `trace spans
// [name]` fires Mode "aggregate" (Phase 2), both the bare and
// name-filtered forms.
func TestConfigureCore_TraceBuiltin_SpansAggregate(t *testing.T) {
	got := runTraceLine(t, "trace --spans")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	if got[0].Mode != "aggregate" {
		t.Errorf("Mode: got %q want aggregate", got[0].Mode)
	}
	if got[0].Pipeline != "trace --spans" {
		t.Errorf("Pipeline: got %q want %q", got[0].Pipeline, "trace --spans")
	}
	if got[0].Title != "all spans" {
		t.Errorf("Title: got %q want %q", got[0].Title, "all spans")
	}

	got = runTraceLine(t, "trace --spans --name coordinator.Execute")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	if got[0].Mode != "aggregate" {
		t.Errorf("Mode: got %q want aggregate", got[0].Mode)
	}
	if got[0].Pipeline != "trace --spans --name coordinator.Execute" {
		t.Errorf("Pipeline: got %q want %q", got[0].Pipeline, "trace --spans --name coordinator.Execute")
	}
	if got[0].Title != "coordinator.Execute" {
		t.Errorf("Title: got %q want coordinator.Execute", got[0].Title)
	}
}

// TestConfigureCore_TraceBuiltin_NoOpWhenNil confirms that when
// Shell.Trace is nil, ConfigureCore doesn't register the `trace`
// builtin at all — the CLI path (spanview.Register /
// RenderTraceIfApplicable) stays untouched.
func TestConfigureCore_TraceBuiltin_NoOpWhenNil(t *testing.T) {
	var registered bool
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	(&Shell{User: "alice"}).ConfigureCore(core)
	core.RegisterBuiltin("trace", func(c *shellcore.ShellCore, args []string) bool {
		registered = true
		return true
	})

	for _, r := range "trace --id abc123" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if !registered {
		t.Fatal("test's own builtin should have been free to register 'trace' — ConfigureCore must not have claimed it when Shell.Trace is nil")
	}
}

// A bare `trace` (the recent-traces list) fires Mode "list"; a scope narrows it.
func TestConfigureCore_TraceBuiltin_List(t *testing.T) {
	for line, want := range map[string]TraceRequest{
		"trace":          {Pipeline: "trace", Title: "recent traces", Mode: "list"},
		"trace acme/api": {Pipeline: "trace acme/api", Title: "recent traces in acme/api", Mode: "list"},
	} {
		got := runTraceLine(t, line)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q: got %+v, want [%+v]", line, got, want)
		}
	}
}

// Shapes the browser has no modal for fall through to normal dispatch.
func TestConfigureCore_TraceBuiltin_FallsThrough(t *testing.T) {
	for _, line := range []string{"trace --spans --name x --samples", "trace | jq .lines", "trace --spans | jq .name", "trace --bogus"} {
		if got := runTraceLine(t, line); len(got) != 0 {
			t.Errorf("%q should not open a trace view, got %+v", line, got)
		}
	}
}

// `trace --help` shows the manual page, as `alert --help` does, and is not
// taken for a trace command.
func TestConfigureCore_TraceHelpShowsTheManual(t *testing.T) {
	var out strings.Builder
	var requests []TraceRequest
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { out.Write(b) },
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Trace: func(r TraceRequest) { requests = append(requests, r) }}
	s.ConfigureCore(core)
	for _, r := range "trace --help" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if len(requests) != 0 {
		t.Errorf("--help opened a trace view: %+v", requests)
	}
	if !strings.Contains(out.String(), "TRACE(1)") || !strings.Contains(out.String(), "--spans") {
		t.Errorf("expected the trace manual page, got:\n%s", out.String())
	}
}
