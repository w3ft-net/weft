package shellcore

import (
	"testing"
)

// TestBinFunctionDispatchedByName verifies the shellcore→server contract:
// when a user types a /bin function name at the prompt, shellcore dispatches
// the invocation verbatim so the server can resolve it.
//
// In the old design shellcore inlined the script body; in the new one the
// server pre-expands bash bodies (for planner visibility) and warp resolves
// at execution time. Either way, what shellcore must NOT do is swallow the
// command — it must reach the server unchanged.
func TestBinFunctionDispatchedByName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bare name", "check_errors", "check_errors"},
		{"bare name with arg", "check_errors disk.log", "check_errors disk.log"},
		{"relative path", "bin/check_errors", "bin/check_errors"},
		{"absolute path", "/bin/check_errors", "/bin/check_errors"},
		{"with trailing compute", "check_errors | wc -l", "check_errors | wc -l"},
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

			if dispatched != tc.want {
				t.Errorf("dispatched = %q, want %q", dispatched, tc.want)
			}
		})
	}
}

// TestBinFunction_UnknownNamePassesThrough — if the first word isn't a /bin
// entry, shellcore dispatches it unchanged (the server will return an error).
func TestBinFunction_UnknownNamePassesThrough(t *testing.T) {
	var dispatched string

	fs := &testMemFS{files: map[string]string{}}
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

	for _, r := range "nonexistent_script hello" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if dispatched != "nonexistent_script hello" {
		t.Errorf("dispatched = %q, want pass-through", dispatched)
	}
}
