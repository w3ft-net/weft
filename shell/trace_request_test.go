package shell

import (
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
	got := runTraceLine(t, "trace abc123")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	r := got[0]
	if r.Mode != "gap" {
		t.Errorf("Mode: got %q want gap", r.Mode)
	}
	if r.Pipeline != "trace abc123" {
		t.Errorf("Pipeline: got %q want %q", r.Pipeline, "trace abc123")
	}
	if r.Title != "abc123" {
		t.Errorf("Title: got %q want abc123", r.Title)
	}
}

// TestConfigureCore_TraceBuiltin_SpansFlag verifies `trace <id>
// --spans` fires Mode "rollup" (Phase 1) with the flag stripped from
// the re-run pipeline, matching spanview.ParseSpansLine's own shape.
func TestConfigureCore_TraceBuiltin_SpansFlag(t *testing.T) {
	got := runTraceLine(t, "trace abc123 --spans")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	r := got[0]
	if r.Mode != "rollup" {
		t.Errorf("Mode: got %q want rollup", r.Mode)
	}
	if r.Pipeline != "trace abc123" {
		t.Errorf("Pipeline: got %q want %q", r.Pipeline, "trace abc123")
	}
	if r.Title != "abc123" {
		t.Errorf("Title: got %q want abc123", r.Title)
	}
}

// TestConfigureCore_TraceBuiltin_SpansAggregate verifies `trace spans
// [name]` fires Mode "aggregate" (Phase 2), both the bare and
// name-filtered forms.
func TestConfigureCore_TraceBuiltin_SpansAggregate(t *testing.T) {
	got := runTraceLine(t, "trace spans")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	if got[0].Mode != "aggregate" {
		t.Errorf("Mode: got %q want aggregate", got[0].Mode)
	}
	if got[0].Pipeline != "trace spans" {
		t.Errorf("Pipeline: got %q want %q", got[0].Pipeline, "trace spans")
	}
	if got[0].Title != "all spans" {
		t.Errorf("Title: got %q want %q", got[0].Title, "all spans")
	}

	got = runTraceLine(t, "trace spans coordinator.Execute")
	if len(got) != 1 {
		t.Fatalf("expected 1 trace request, got %d", len(got))
	}
	if got[0].Mode != "aggregate" {
		t.Errorf("Mode: got %q want aggregate", got[0].Mode)
	}
	if got[0].Pipeline != "trace spans coordinator.Execute" {
		t.Errorf("Pipeline: got %q want %q", got[0].Pipeline, "trace spans coordinator.Execute")
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

	for _, r := range "trace abc123" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if !registered {
		t.Fatal("test's own builtin should have been free to register 'trace' — ConfigureCore must not have claimed it when Shell.Trace is nil")
	}
}
