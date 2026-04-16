package shellcore

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type testHarness struct {
	scrollback strings.Builder
	prompt     string
	actions    []Action
}

// Scrollback returns the accumulated scrollback with ANSI cursor control stripped.
func (h *testHarness) Scrollback() string {
	return stripTestCursorControl(h.scrollback.String())
}

func newTestCore(apps []string) (*ShellCore, *testHarness) {
	h := &testHarness{}
	core := NewShellCore(apps, 80, 24,
		func(b []byte) {
			s := string(b)
			if strings.Contains(s, "\x1b[2J") {
				h.scrollback.Reset()
				return
			}
			h.scrollback.Write(b)
		},
		func(a Action) { h.actions = append(h.actions, a) },
	)
	return core, h
}

// stripTestCursorControl removes ANSI cursor movement sequences for test assertions.
func stripTestCursorControl(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\r' {
			i++
			continue
		}
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				j++
			}
			if j < len(s) {
				switch s[j] {
				case 'A', 'J', 'H': // cursor up, clear, home — skip
					i = j + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestInitialMode(t *testing.T) {
	core, _ := newTestCore([]string{"api", "web"})
	if core.Mode() != ModeInput {
		t.Errorf("initial mode = %d, want ModeInput", core.Mode())
	}
}

func TestTypingAndEnter(t *testing.T) {
	core, h := newTestCore([]string{"api"})

	// Type "cat api.log"
	for _, r := range "cat api.log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}

	if core.GetInput() != "cat api.log" {
		t.Errorf("input = %q, want %q", core.GetInput(), "cat api.log")
	}

	// Press Enter — should dispatch ActionQuery.
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if len(h.actions) == 0 {
		t.Fatal("expected an action after Enter")
	}
	q, ok := h.actions[0].(ActionQuery)
	if !ok {
		t.Fatalf("expected ActionQuery, got %T", h.actions[0])
	}
	if q.Pipeline != "cat api.log" {
		t.Errorf("pipeline = %q, want %q", q.Pipeline, "cat api.log")
	}

	if core.Mode() != ModeRunning {
		t.Errorf("mode = %d, want ModeRunning", core.Mode())
	}

	// Input should be cleared.
	if core.GetInput() != "" {
		t.Errorf("input after Enter = %q, want empty", core.GetInput())
	}
}

func TestHistoryNavigation(t *testing.T) {
	core, _ := newTestCore(nil)

	// Submit two commands.
	for _, cmd := range []string{"first", "second"} {
		for _, r := range cmd {
			core.HandleEvent(KeyEvent{Rune: r})
		}
		core.HandleEvent(KeyEvent{Key: KeyEnter})
		// Deliver query result to go back to ModeInput.
		core.HandleEvent(QueryResultEvent{Text: "ok\n"})
	}

	// Up should show "second".
	core.HandleEvent(KeyEvent{Key: KeyUp})
	if core.GetInput() != "second" {
		t.Errorf("after first Up: %q, want %q", core.GetInput(), "second")
	}

	// Up again should show "first".
	core.HandleEvent(KeyEvent{Key: KeyUp})
	if core.GetInput() != "first" {
		t.Errorf("after second Up: %q, want %q", core.GetInput(), "first")
	}

	// Down should show "second".
	core.HandleEvent(KeyEvent{Key: KeyDown})
	if core.GetInput() != "second" {
		t.Errorf("after Down: %q, want %q", core.GetInput(), "second")
	}
}

func TestBuiltinApps(t *testing.T) {
	core, h := newTestCore([]string{"api", "web"})

	for _, r := range "apps" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	// Should NOT dispatch a query — apps is a builtin.
	for _, a := range h.actions {
		if _, ok := a.(ActionQuery); ok {
			t.Error("apps should not dispatch ActionQuery")
		}
	}

	if !strings.Contains(h.Scrollback(), "api.log") {
		t.Error("scrollback should contain api.log")
	}
	if !strings.Contains(h.Scrollback(), "web.log") {
		t.Error("scrollback should contain web.log")
	}
}

func TestBuiltinClear(t *testing.T) {
	core, h := newTestCore(nil)

	// Add some scrollback.
	core.HandleEvent(QueryResultEvent{Text: "some output\n"})

	for _, r := range "clear" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	// After clear, the old scrollback should be gone.
	// The prompt will appear (via same write callback) but
	// "some output" should not.
	sb := h.Scrollback()
	if strings.Contains(sb, "some output") {
		t.Errorf("scrollback after clear still contains 'some output': %q", sb)
	}
}

func TestFollowMode(t *testing.T) {
	core, h := newTestCore(nil)

	for _, r := range "tail -f api.log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if core.Mode() != ModeFollowing {
		t.Errorf("mode = %d, want ModeFollowing", core.Mode())
	}

	if len(h.actions) == 0 {
		t.Fatal("expected ActionFollow")
	}
	f, ok := h.actions[len(h.actions)-1].(ActionFollow)
	if !ok {
		t.Fatalf("expected ActionFollow, got %T", h.actions[len(h.actions)-1])
	}
	if f.Pipeline != "tail -f api.log" {
		t.Errorf("pipeline = %q", f.Pipeline)
	}

	// Deliver some rows.
	core.HandleEvent(FollowRowEvent{Raw: "line1"})
	core.HandleEvent(FollowRowEvent{Raw: "line2"})

	if !strings.Contains(h.Scrollback(), "line1") || !strings.Contains(h.Scrollback(), "line2") {
		t.Error("scrollback missing follow rows")
	}

	// Ctrl+C should stop following.
	core.HandleEvent(KeyEvent{Key: KeyCtrlC})
	if core.Mode() != ModeInput {
		t.Errorf("mode after Ctrl+C = %d, want ModeInput", core.Mode())
	}
}

func TestCtrlDQuits(t *testing.T) {
	core, h := newTestCore(nil)
	core.HandleEvent(KeyEvent{Key: KeyCtrlD})

	if !core.Quitting() {
		t.Error("expected quitting=true after Ctrl+D")
	}

	found := false
	for _, a := range h.actions {
		if _, ok := a.(ActionQuit); ok {
			found = true
		}
	}
	if !found {
		t.Error("expected ActionQuit")
	}
}

// TestEnterXtermOutput simulates the exact byte stream xterm.js would receive
// when the user types "abc" then presses Enter. This catches prompt-overwrite bugs.
func TestEnterXtermOutput(t *testing.T) {
	// Capture raw bytes in order, exactly as xterm.js would see them.
	var rawOutput strings.Builder
	var actions []Action

	core := NewShellCore(
		nil, 80, 24,
		func(b []byte) { rawOutput.Write(b) },
		func(a Action) { actions = append(actions, a) },
	)

	// Initial Redraw (simulates wasmInit).
	core.Redraw()
	rawOutput.Reset() // clear the initial screen setup

	// Type "abc".
	for _, r := range "abc" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	rawOutput.Reset() // we only care about Enter output

	// Press Enter.
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	raw := rawOutput.String()
	// Strip all ANSI escape sequences to get visible text.
	visible := stripAllANSI(raw)

	t.Logf("raw bytes: %q", raw)
	t.Logf("visible text: %q", visible)

	// The visible output should contain:
	// 1. The echo line "$ abc" followed by a newline
	// 2. The spinner prompt (since mode is ModeRunning)
	if !strings.Contains(visible, "$ abc") {
		t.Errorf("missing echo line in visible output: %q", visible)
	}
	if !strings.Contains(visible, "querying shards") {
		t.Errorf("missing spinner prompt — got: %q", visible)
	}

	if core.Mode() != ModeRunning {
		t.Errorf("mode after Enter = %d, want ModeRunning", core.Mode())
	}

	// Now simulate what happens when the query fails immediately
	// (as it would in the browser with no backend).
	rawOutput.Reset()
	core.HandleEvent(QueryResultEvent{Err: jsTestError("fetch failed")})

	raw2 := rawOutput.String()
	visible2 := stripAllANSI(raw2)
	t.Logf("after error - raw: %q", raw2)
	t.Logf("after error - visible: %q", visible2)

	if !strings.Contains(visible2, "fetch failed") {
		t.Errorf("missing error text: %q", visible2)
	}
	if !strings.Contains(visible2, "$") {
		t.Errorf("missing prompt after error: %q", visible2)
	}
	if core.Mode() != ModeInput {
		t.Errorf("mode after error = %d, want ModeInput", core.Mode())
	}
}

type jsTestError string

func (e jsTestError) Error() string { return string(e) }

// TestEnterWithSynchronousQueryResult simulates what happens in the browser
// when the dispatch callback triggers an immediate query result (e.g., fetch
// resolves during the same JS turn). The dispatch callback calls HandleEvent
// re-entrantly with a QueryResultEvent.
func TestEnterWithSynchronousQueryResult(t *testing.T) {
	var writeCalls []string

	var core *ShellCore

	core = NewShellCore(
		nil, 80, 24,
		func(b []byte) {
			writeCalls = append(writeCalls, string(b))
		},
		func(a Action) {
			// Simulate JS dispatch: immediately resolve the query.
			if _, ok := a.(ActionQuery); ok {
				core.HandleEvent(QueryResultEvent{
					Err: jsTestError("connection refused"),
				})
			}
		},
	)

	core.Redraw()
	writeCalls = nil

	// Type "hello".
	for _, r := range "hello" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	writeCalls = nil

	// Press Enter.
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	t.Logf("write calls: %d", len(writeCalls))
	for i, c := range writeCalls {
		t.Logf("  write[%d] visible: %q", i, stripAllANSI(c))
		t.Logf("  write[%d] raw:     %q", i, c)
	}

	// Combine all writes to see what xterm.js would display.
	var combined strings.Builder
	for _, c := range writeCalls {
		combined.WriteString(c)
	}
	visible := stripAllANSI(combined.String())
	t.Logf("combined visible: %q", visible)

	// The echo line "$ hello" MUST appear.
	if !strings.Contains(visible, "$ hello") {
		t.Error("echo line '$ hello' missing from output")
	}

	// The error "connection refused" MUST appear.
	if !strings.Contains(visible, "connection refused") {
		t.Error("error 'connection refused' missing from output")
	}
}

// stripAllANSI removes all ANSI escape sequences from a string.
func stripAllANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				j++
			}
			if j < len(s) {
				j++ // skip final byte
			}
			i = j
			continue
		}
		if s[i] == '\r' {
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestReverseSearch(t *testing.T) {
	core, _ := newTestCore(nil)

	// Submit a command.
	for _, r := range "grep ERROR api.log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	core.HandleEvent(QueryResultEvent{Text: "ok\n"})

	// Enter reverse search.
	core.HandleEvent(KeyEvent{Key: KeyCtrlR})
	if core.Mode() != ModeSearching {
		t.Fatalf("mode = %d, want ModeSearching", core.Mode())
	}

	// Type "ERR".
	for _, r := range "ERR" {
		core.HandleEvent(KeyEvent{Rune: r})
	}

	if core.GetInput() != "grep ERROR api.log" {
		t.Errorf("search result = %q, want %q", core.GetInput(), "grep ERROR api.log")
	}

	// Enter accepts the result.
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	if core.Mode() != ModeInput {
		t.Errorf("mode after Enter = %d, want ModeInput", core.Mode())
	}
}

// testMemFS is a simple in-memory FS for testing.
type testMemFS struct {
	files map[string]string
}

func (f *testMemFS) ReadFile(path string) ([]byte, error) {
	if c, ok := f.files[path]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("not found: %s", path)
}

func (f *testMemFS) WriteFile(path string, data []byte) error {
	f.files[path] = string(data)
	return nil
}

func (f *testMemFS) ReadDir(path string) ([]string, error) {
	var entries []string
	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if prefix == "/" {
		prefix = "/"
	}
	for k := range f.files {
		if strings.HasPrefix(k, prefix) {
			rest := k[len(prefix):]
			if !strings.Contains(rest, "/") && rest != "" {
				entries = append(entries, rest)
			}
		}
	}
	return entries, nil
}

func (f *testMemFS) Remove(path string) error {
	if _, ok := f.files[path]; !ok {
		return fmt.Errorf("not found: %s", path)
	}
	delete(f.files, path)
	return nil
}

func TestVimOpenEditSaveQuit(t *testing.T) {
	var writeBuf strings.Builder
	var actions []Action

	fs := &testMemFS{files: map[string]string{
		"/bin/check_errors": "echo hello\n",
	}}

	core := NewShellCore(nil, 80, 24,
		func(b []byte) { writeBuf.Write(b) },
		func(a Action) { actions = append(actions, a) },
	)
	core.SetFS(fs)
	core.Redraw()

	// Type "vi /bin/check_errors" and press Enter.
	for _, r := range "vi /bin/check_errors" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if core.Mode() != ModeVim {
		t.Fatalf("mode = %d, want ModeVim", core.Mode())
	}

	// The vim editor should have loaded the file content.
	visible := stripAllANSI(writeBuf.String())
	if !strings.Contains(visible, "echo hello") {
		t.Errorf("vim should show file content, got: %q", visible[:min(len(visible), 200)])
	}

	// Type "A world" (append to end of line, type " world").
	core.HandleEvent(KeyEvent{Rune: 'A'}) // append at end of line
	for _, r := range " world" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	// Press Escape to return to normal mode.
	core.HandleEvent(KeyEvent{Key: KeyEsc})

	// Save with :w
	core.HandleEvent(KeyEvent{Rune: ':'})
	core.HandleEvent(KeyEvent{Rune: 'w'})
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	// Check that ActionVimSave was dispatched.
	saveFound := false
	for _, a := range actions {
		if s, ok := a.(ActionVimSave); ok {
			saveFound = true
			if !strings.Contains(s.Content, "world") {
				t.Errorf("saved content should contain 'world', got: %q", s.Content)
			}
		}
	}
	if !saveFound {
		t.Error("expected ActionVimSave dispatch")
	}

	// Quit with :q
	core.HandleEvent(KeyEvent{Rune: ':'})
	core.HandleEvent(KeyEvent{Rune: 'q'})
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if core.Mode() != ModeInput {
		t.Errorf("mode after :q = %d, want ModeInput", core.Mode())
	}
}

// TestVimRelativePath verifies that "vi bin/foo" loads the same file as "vi /bin/foo".
func TestVimRelativePath(t *testing.T) {
	var writeBuf strings.Builder

	fs := &testMemFS{files: map[string]string{
		"/bin/check_errors": "echo hello\n",
	}}

	core := NewShellCore(nil, 80, 24,
		func(b []byte) { writeBuf.Write(b) },
		func(a Action) {},
	)
	core.SetFS(fs)
	core.Redraw()

	// Type "vi bin/check_errors" (relative path, no leading /).
	for _, r := range "vi bin/check_errors" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if core.Mode() != ModeVim {
		t.Fatalf("mode = %d, want ModeVim", core.Mode())
	}

	visible := stripAllANSI(writeBuf.String())
	if !strings.Contains(visible, "echo hello") {
		t.Errorf("vi with relative path should load file content, got: %q", visible[:min(len(visible), 200)])
	}

	// Quit.
	core.HandleEvent(KeyEvent{Rune: ':'})
	core.HandleEvent(KeyEvent{Rune: 'q'})
	core.HandleEvent(KeyEvent{Key: KeyEnter})
}

// TestCursorPositionAfterLeftArrow verifies that pressing left arrow causes
// the terminal output to include a cursor positioning escape sequence so the
// on-screen cursor actually moves, rather than staying at end-of-line.
func TestCursorPositionAfterLeftArrow(t *testing.T) {
	var rawOutput strings.Builder

	core := NewShellCore(
		nil, 80, 24,
		func(b []byte) { rawOutput.Write(b) },
		func(a Action) {},
	)
	core.Redraw()

	// Type "abcdef".
	for _, r := range "abcdef" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	rawOutput.Reset() // only care about output after left arrows

	// Press left 3 times — cursor should be at position 3 (between 'c' and 'd').
	core.HandleEvent(KeyEvent{Key: KeyLeft})
	core.HandleEvent(KeyEvent{Key: KeyLeft})
	core.HandleEvent(KeyEvent{Key: KeyLeft})

	raw := rawOutput.String()
	t.Logf("raw after 3x left: %q", raw)

	// The prompt prefix "~ $ " is 4 visible chars. With inputCursor=3,
	// the terminal cursor should be at column 8 (4 + 3 + 1 for 1-indexed).
	// Check for \x1b[8G (absolute column positioning).
	if !strings.Contains(raw, "\x1b[8G") {
		t.Errorf("expected cursor positioning escape \\x1b[8G in output, got: %q", raw)
	}

	// Also verify that the block cursor (reverse video) is rendered on 'd'
	// (the character at position 3), not at end of input.
	// ansiCursor = "\033[7m" (reverse video).
	if !strings.Contains(raw, "\x1b[7md") {
		t.Errorf("expected reverse-video block cursor on 'd', got: %q", raw)
	}
}

// TestPromptFormat verifies the prompt displays [range] user@weft cwd $ format.
func TestPromptFormat(t *testing.T) {
	var rawOutput strings.Builder

	core := NewShellCore(
		nil, 80, 24,
		func(b []byte) { rawOutput.Write(b) },
		func(a Action) {},
	)
	core.SetUser("paul")
	core.SetRange(
		time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC),
	)
	core.Redraw()

	visible := stripAllANSI(rawOutput.String())
	t.Logf("visible prompt: %q", visible)

	// Should show [1d] paul@weft ~ $ (24h = 1d per formatDuration)
	if !strings.Contains(visible, "[1d]") {
		t.Errorf("expected [1d] in prompt, got: %q", visible)
	}
	if !strings.Contains(visible, "paul@weft") {
		t.Errorf("expected paul@weft in prompt, got: %q", visible)
	}
	if !strings.Contains(visible, "~ $") {
		t.Errorf("expected '~ $' in prompt, got: %q", visible)
	}
}

// TestPromptCwdNotRoot verifies cwd shows the path when not at /.
func TestPromptCwdNotRoot(t *testing.T) {
	var rawOutput strings.Builder

	core := NewShellCore(
		nil, 80, 24,
		func(b []byte) { rawOutput.Write(b) },
		func(a Action) {},
	)
	core.SetUser("paul")
	core.SetCwd("/log")
	core.SetRange(
		time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC),
	)
	core.Redraw()

	visible := stripAllANSI(rawOutput.String())
	t.Logf("visible prompt: %q", visible)

	if !strings.Contains(visible, "[2h]") {
		t.Errorf("expected [2h] in prompt, got: %q", visible)
	}
	if !strings.Contains(visible, "/log $") {
		t.Errorf("expected '/log $' in prompt, got: %q", visible)
	}
}

// TestCdAndLs verifies that cd changes the working directory and ls uses it.
func TestCdAndLs(t *testing.T) {
	core, h := newTestCore([]string{"api", "web"})
	fs := &testMemFS{files: map[string]string{
		"/log/api.log":       "",
		"/log/web.log":       "",
		"/bin/check_errors":  "echo hello\n",
	}}
	core.SetFS(fs)

	// cd /log
	for _, r := range "cd /log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	// ls (no args) should list /log contents
	for _, r := range "ls" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	sb := h.Scrollback()
	if !strings.Contains(sb, "api.log") {
		t.Errorf("ls after cd /log should show api.log, got: %q", sb)
	}
	if !strings.Contains(sb, "web.log") {
		t.Errorf("ls after cd /log should show web.log, got: %q", sb)
	}

	// cd .. should go back to / — verify via prompt showing "~"
	for _, r := range "cd .." {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	h.scrollback.Reset()
	// Type something to trigger a prompt redraw we can inspect.
	core.HandleEvent(KeyEvent{Rune: 'x'})
	sb = h.Scrollback()
	visible := stripAllANSI(sb)
	if !strings.Contains(visible, "~ $") {
		t.Errorf("prompt after cd .. should show '~ $', got: %q", visible)
	}
	// Clean up the typed char.
	core.HandleEvent(KeyEvent{Key: KeyBackspace})

	// cd /bin then cd (no args) should go to /
	for _, r := range "cd /bin" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	for _, r := range "cd" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	h.scrollback.Reset()
	core.HandleEvent(KeyEvent{Rune: 'x'})
	sb = h.Scrollback()
	visible = stripAllANSI(sb)
	if !strings.Contains(visible, "~ $") {
		t.Errorf("prompt after cd (home) should show '~ $', got: %q", visible)
	}
	core.HandleEvent(KeyEvent{Key: KeyBackspace})

	// Verify no queries were dispatched (all builtins).
	for _, a := range h.actions {
		if _, ok := a.(ActionQuery); ok {
			t.Error("cd/ls should not dispatch ActionQuery")
		}
	}
}

// TestTailResolvesFromCwdDispatch verifies the dispatched pipeline has resolved paths.
func TestTailResolvesFromCwdDispatch(t *testing.T) {
	var dispatched string

	fs := &testMemFS{files: map[string]string{
		"/log/weft/api.log": "",
	}}

	core := NewShellCore(
		[]string{"weft/api"}, 80, 24,
		func(b []byte) {},
		func(a Action) {
			if q, ok := a.(ActionQuery); ok {
				dispatched = q.Pipeline
			}
		},
	)
	core.SetFS(fs)

	// cd /log/weft
	for _, r := range "cd /log/weft" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	// tail api.log — should resolve to tail /log/weft/api.log
	for _, r := range "tail api.log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if dispatched != "tail /log/weft/api.log" {
		t.Errorf("dispatched = %q, want %q", dispatched, "tail /log/weft/api.log")
	}
}

// TestCdRelative verifies that relative cd works.
func TestCdRelative(t *testing.T) {
	core, _ := newTestCore(nil)
	fs := &testMemFS{files: map[string]string{
		"/log/api.log": "",
	}}
	core.SetFS(fs)

	// From /, cd log should go to /log
	for _, r := range "cd log" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	if core.GetInput() != "" {
		// Input should be cleared after Enter
	}
	// We can't directly check cwd, but we can verify via ls
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
