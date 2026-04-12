package shellcore

import (
	"strings"
	"testing"
)

// TestBinFunctionExecutable verifies that a function saved in /bin/ can be
// called by name from the shell prompt, like a real executable in PATH.
// e.g., if /bin/check_errors contains "cat /log/api.log | grep ERROR | wc -l",
// then typing "check_errors" at the prompt should dispatch that pipeline.
func TestBinFunctionExecutable(t *testing.T) {
	var dispatched string

	fs := &testMemFS{files: map[string]string{
		"/bin/check_errors": "cat /log/vimgym.log | grep ERROR | wc -l\n",
	}}

	core := NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) {},
		func(a Action) {
			if q, ok := a.(ActionQuery); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(fs)
	core.Redraw()

	// Type "check_errors" and press Enter.
	for _, r := range "check_errors" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if dispatched == "" {
		t.Fatal("typing a /bin/ function name did not dispatch a query")
	}
	if !strings.Contains(dispatched, "vimgym.log") {
		t.Errorf("dispatched pipeline = %q, want the function's content", dispatched)
	}
}

// TestBinFunctionInCronWatch verifies that a cron watch referencing a /bin/
// function by name would resolve the function's content for execution.
// The cron executor reads the watch pipeline, and if it's a bare function name,
// it should resolve it from /bin/ and execute the contents.
func TestBinFunctionInCronWatch(t *testing.T) {
	var dispatched string

	fs := &testMemFS{files: map[string]string{
		"/bin/check_errors": "cat /log/vimgym.log | grep ERROR | wc -l\n",
	}}

	core := NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) {},
		func(a Action) {
			if q, ok := a.(ActionQuery); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(fs)
	core.Redraw()

	// Simulate what the cron executor would do: execute "check_errors" as a pipeline.
	// The shell should resolve it from /bin/ just like typing it at the prompt.
	for _, r := range "check_errors" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if dispatched == "" {
		t.Fatal("function name did not resolve from /bin/")
	}
	if !strings.Contains(dispatched, "grep ERROR") {
		t.Errorf("dispatched = %q, expected function body with 'grep ERROR'", dispatched)
	}
}

// TestBinFunctionResolvesLogPaths verifies that a function containing
// "cat /log/vimgym.log | ..." dispatches a pipeline the coordinator understands
// (with /log/ stripped), not the raw FS path.
func TestBinFunctionResolvesLogPaths(t *testing.T) {
	var dispatched string

	fs := &testMemFS{files: map[string]string{
		"/bin/check_errors": "cat /log/vimgym.log | grep ERROR | wc -l\n",
	}}

	core := NewShellCore(
		[]string{"vimgym"}, 80, 24,
		func(b []byte) {},
		func(a Action) {
			if q, ok := a.(ActionQuery); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(fs)
	core.Redraw()

	for _, r := range "check_errors" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if dispatched == "" {
		t.Fatal("no query dispatched")
	}
	// The planner now handles /log/ paths natively, so the dispatched pipeline
	// should contain the full path as written in the function.
	if !strings.Contains(dispatched, "vimgym.log") {
		t.Errorf("dispatched pipeline missing vimgym.log: %q", dispatched)
	}
}

// TestBinFunctionAbsolutePath verifies that "/bin/foo" and "bin/foo" both
// resolve the function, not just "foo".
func TestBinFunctionAbsolutePath(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"bare name", "check_errors"},
		{"relative path", "bin/check_errors"},
		{"absolute path", "/bin/check_errors"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched string

			fs := &testMemFS{files: map[string]string{
				"/bin/check_errors": "cat /log/vimgym.log | grep ERROR | wc -l\n",
			}}

			core := NewShellCore(
				[]string{"vimgym"}, 80, 24,
				func(b []byte) {},
				func(a Action) {
					if q, ok := a.(ActionQuery); ok {
						dispatched = q.Pipeline
					}
				},
			)
			core.SetFS(fs)
			core.Redraw()

			for _, r := range tc.input {
				core.HandleEvent(KeyEvent{Rune: r})
			}
			core.HandleEvent(KeyEvent{Key: KeyEnter})

			if dispatched == "" {
				t.Fatalf("%q did not dispatch a query", tc.input)
			}
			if !strings.Contains(dispatched, "grep ERROR") {
				t.Errorf("%q dispatched %q, want function body", tc.input, dispatched)
			}
		})
	}
}
