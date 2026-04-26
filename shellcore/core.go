package shellcore

import (
	"fmt"
	"strings"
	"time"
)

// terminalCommands are commands recognised as a terminal pager suffix
// in a shell pipeline. Today only `less` is wired through ModeLess;
// `more` would need its own path.
var terminalCommands = map[string]bool{
	"less": true,
}

// ShellCore is the shared shell state machine.
// It is synchronous and passive — the renderer calls HandleEvent(),
// and ShellCore writes ANSI to a single callback and dispatches actions.
type ShellCore struct {
	history   *History
	completer *Completer
	mode      Mode

	write    func([]byte)
	dispatch func(Action)

	width, height int
	inputBuf      []rune
	inputCursor   int
	spinnerFrame  int
	scrollback    strings.Builder
	lastFlushedLen  int // byte offset of last flushed scrollback content
	prevPromptLines int // number of terminal lines the last prompt occupied

	// Reverse search state.
	searchBuf string
	searchIdx int

	// Vim editor state (active when mode == ModeVim).
	vim *vimState

	// Pager state (active when mode == ModeLess).
	less *lessState

	// Live-chart state (active when mode == ModeChart).
	chart *chartState

	// pendingLess is set when the user typed `... | less`; the query
	// runs stripped of the terminal stage and the result enters the
	// pager on arrival instead of landing in scrollback.
	pendingLess bool

	// tierLimits provides the runtime tier→watch-cap table for
	// `help cron`. nil = fall back to a generic "see your plan" line.
	tierLimits TierLimitsProvider

	// Stdin capture state (cat > /path mode).
	capture *captureState

	// Virtual filesystem for vim file editing (optional).
	// If nil, `vi` commands will error.
	shellFS FS

	// Set by execBuiltin("exit"/"quit") — renderer should exit.
	quitting bool

	// maxScrollbackLines bounds scrollback to prevent memory issues.
	maxScrollbackLines int

	// Query time range (displayed by `range`, modified by `range <spec>`).
	rangeStart time.Time
	rangeEnd   time.Time

	// Prompt identity fields.
	user     string // e.g. "alice" — shown as alice@<customer> in prompt
	customer string // e.g. "acme"  — defaults to "weft" when empty
	cwd  string // virtual working directory, "/" = home = "~"
}

// NewShellCore creates a new core with a single write callback.
func NewShellCore(apps []string, width, height int, write func([]byte), dispatch func(Action)) *ShellCore {
	return &ShellCore{
		history:            &History{},
		completer:          NewCompleter(apps),
		mode:               ModeInput,
		write:              write,
		dispatch:           dispatch,
		width:              width,
		height:             height,
		maxScrollbackLines: 10000,
		cwd:                "/",
	}
}

// Mode returns the current shell mode.
func (c *ShellCore) Mode() Mode { return c.mode }

// Quitting returns true if the shell has been asked to exit.
func (c *ShellCore) Quitting() bool { return c.quitting }

// SpinnerFrame returns the current spinner frame index.
func (c *ShellCore) SpinnerFrame() int { return c.spinnerFrame }

// CompleterActive returns whether the completer menu is visible.
func (c *ShellCore) CompleterActive() bool { return c.completer.Active() }

// SetFS sets the virtual filesystem for vim file editing.
func (c *ShellCore) SetFS(fs FS) { c.shellFS = fs }

// GetFS returns the virtual filesystem.
func (c *ShellCore) GetFS() FS { return c.shellFS }

// SetRange sets the current query time range.
func (c *ShellCore) SetRange(start, end time.Time) {
	c.rangeStart = start
	c.rangeEnd = end
}

// SetHistoryStore sets the persistence backend for command history.
func (c *ShellCore) SetHistoryStore(store HistoryStore) { c.history.SetStore(store) }

// SetUser sets the username shown in the prompt (e.g. "alice" → "alice@<customer>").
func (c *ShellCore) SetUser(user string) { c.user = user }

// SetCustomer sets the customer/team slug shown after the @ in the
// prompt (e.g. "acme" → "alice@acme"). Empty falls back to "weft".
func (c *ShellCore) SetCustomer(customer string) { c.customer = customer }

// SetCwd sets the virtual working directory shown in the prompt.
func (c *ShellCore) SetCwd(cwd string) { c.cwd = cwd }

// promptInfo builds the PromptInfo from current state.
func (c *ShellCore) promptInfo() PromptInfo {
	var rangeLabel string
	if !c.rangeStart.IsZero() && !c.rangeEnd.IsZero() {
		rangeLabel = formatDuration(c.rangeEnd.Sub(c.rangeStart))
	}
	return PromptInfo{
		RangeLabel: rangeLabel,
		User:       c.user,
		Customer:   c.customer,
		Cwd:        c.cwd,
	}
}

// SetInput replaces the current input buffer (for saved query injection).
func (c *ShellCore) SetInput(text string) {
	c.inputBuf = []rune(text)
	c.inputCursor = len(c.inputBuf)
	c.redrawPrompt()
}

// GetInput returns the current input text.
func (c *ShellCore) GetInput() string {
	return string(c.inputBuf)
}

// Scrollback returns the current scrollback content.
func (c *ShellCore) Scrollback() string {
	return c.scrollback.String()
}

// HandleEvent processes an input event and updates state.
func (c *ShellCore) HandleEvent(ev Event) {
	switch ev := ev.(type) {
	case KeyEvent:
		c.handleKey(ev)
	case ResizeEvent:
		c.width = ev.Width
		c.height = ev.Height
		if c.mode == ModeVim {
			c.renderVim()
		}
		if c.mode == ModeLess && c.less != nil {
			// Re-wrap content at the new width so the viewport stays
			// coherent. Keep the reader at approximately the same
			// logical position by snapping to the nearest wrapped
			// line index derived from the pre-resize ratio.
			prevTotal := len(c.less.lines)
			prevOffset := c.less.offset
			c.less.lines = wrapLines(strings.Join(c.less.lines, "\n"), ev.Width)
			c.less.width = ev.Width
			c.less.height = ev.Height
			if prevTotal > 0 {
				c.less.offset = prevOffset * len(c.less.lines) / prevTotal
			}
			if c.less.query != "" {
				c.computeMatches()
			}
			c.clampOffset()
			c.renderLess()
		}
		if c.mode == ModeChart && c.chart != nil {
			// Re-fit immediately on resize — the chart always paints
			// to the current terminal dimensions, never paginated.
			c.renderChart()
		}
	case QueryResultEvent:
		c.handleQueryResult(ev)
	case FollowRowEvent:
		c.handleFollowRow(ev)
	case FollowDoneEvent:
		c.handleFollowDone(ev)
	}
}

// SpinnerTick advances the spinner frame. Called by the renderer's timer.
// Also drives the chart-mode redraw via chartTick (which throttles
// internally to the user-configured interval).
func (c *ShellCore) SpinnerTick() {
	if c.mode == ModeRunning {
		c.spinnerFrame = (c.spinnerFrame + 1) % len(SpinnerFrames)
		c.redrawPrompt()
	}
	if c.mode == ModeChart {
		c.chartTick()
	}
}

// Redraw forces a full screen repaint from scratch.
func (c *ShellCore) Redraw() {
	// Reset flush state and emit everything.
	c.prevPromptLines = 0
	c.lastFlushedLen = 0
	c.write([]byte("\x1b[2J\x1b[H")) // clear screen + home cursor
	full := c.scrollback.String()
	if len(full) > 0 {
		c.write([]byte(full))
	}
	c.lastFlushedLen = len(full)
	c.redrawPrompt()
}

// WriteStartupBanner writes the logo to scrollback.
func (c *ShellCore) WriteStartupBanner() {
	c.scrollback.WriteString(RenderLogo())
	c.flushScrollback()
}

// ── Key handling ────────────────────────────────────────────

func (c *ShellCore) handleKey(ev KeyEvent) {
	// Vim mode: all keys go to the vim editor.
	if c.mode == ModeVim {
		c.handleVimKey(ev)
		return
	}

	// Less mode: all keys go to the pager.
	if c.mode == ModeLess {
		c.handleLessKey(ev)
		return
	}

	// Chart mode: only exit keys are handled; redraw runs on tick.
	if c.mode == ModeChart {
		c.handleChartKey(ev)
		return
	}

	// Capture mode: accumulating stdin for redirect.
	if c.mode == ModeCapture {
		c.handleCaptureKey(ev)
		return
	}

	// Follow mode: only Ctrl+C stops it.
	if c.mode == ModeFollowing {
		if ev.Key == KeyCtrlC {
			c.mode = ModeInput
			c.dispatch(ActionCancel{})
			c.appendScrollback(DimText("\n[tail stopped]") + "\n")
			c.redrawPrompt()
		}
		return
	}

	// Running mode: only Ctrl+C interrupts.
	if c.mode == ModeRunning {
		if ev.Key == KeyCtrlC {
			c.mode = ModeInput
			c.dispatch(ActionCancel{})
			c.appendScrollback(ErrorText("interrupted") + "\n")
			c.redrawPrompt()
		}
		return
	}

	// Search mode.
	if c.mode == ModeSearching {
		c.handleSearchKey(ev)
		return
	}

	// Normal input mode.
	switch ev.Key {
	case KeyCtrlC, KeyCtrlD:
		c.completer.Accept()
		c.quitting = true
		c.dispatch(ActionQuit{})
		return

	case KeyCtrlR:
		c.completer.Accept()
		c.mode = ModeSearching
		c.searchBuf = ""
		c.searchIdx = c.history.Len() - 1
		c.redrawPrompt()
		return

	case KeyPgUp, KeyPgDown:
		// Scroll events — passed through to renderer.
		return

	case KeyTab:
		newInput, newCursor := c.completer.Tab(c.inputBuf, c.inputCursor, c.shellFS, c.cwd)
		c.inputBuf = newInput
		c.inputCursor = newCursor
		c.redrawPrompt()
		return

	case KeyShiftTab:
		if c.completer.Active() {
			newInput, newCursor := c.completer.ShiftTab(c.inputBuf, c.inputCursor)
			c.inputBuf = newInput
			c.inputCursor = newCursor
			c.redrawPrompt()
		}
		return

	case KeyEsc:
		if c.completer.Active() {
			c.completer.Accept()
			c.redrawPrompt()
			return
		}

	case KeyUp:
		c.completer.Accept()
		if line, ok := c.history.Up(c.GetInput()); ok {
			c.inputBuf = []rune(line)
			c.inputCursor = len(c.inputBuf)
		}
		c.redrawPrompt()
		return

	case KeyDown:
		c.completer.Accept()
		if line, ok := c.history.Down(c.GetInput()); ok {
			c.inputBuf = []rune(line)
			c.inputCursor = len(c.inputBuf)
		}
		c.redrawPrompt()
		return

	case KeyEnter:
		c.completer.Accept()
		line := strings.TrimSpace(c.GetInput())
		c.inputBuf = nil
		c.inputCursor = 0
		c.history.Reset()
		if line == "" {
			c.redrawPrompt()
			return
		}
		c.history.Add(line)
		c.appendScrollback(PromptText(FormatPromptPrefix(c.promptInfo())) + HighlightPipeline(line) + "\n")

		if c.execBuiltin(line) {
			c.redrawPrompt()
			return
		}

		// Check if the command is a /bin/ function.
		if resolved := c.resolveFunction(line); resolved != "" {
			// Run the resolved content through builtins (handles cat /log/ rewriting etc).
			if c.execBuiltin(resolved) {
				c.redrawPrompt()
				return
			}
			c.startQuery(resolved)
			c.redrawPrompt()
			return
		}

		c.startQuery(line)
		c.redrawPrompt()
		return

	case KeyBackspace:
		if c.completer.Active() {
			c.completer.Accept()
		}
		if c.inputCursor > 0 {
			c.inputBuf = append(c.inputBuf[:c.inputCursor-1], c.inputBuf[c.inputCursor:]...)
			c.inputCursor--
		}
		c.redrawPrompt()
		return

	case KeyDelete:
		if c.inputCursor < len(c.inputBuf) {
			c.inputBuf = append(c.inputBuf[:c.inputCursor], c.inputBuf[c.inputCursor+1:]...)
		}
		c.redrawPrompt()
		return

	case KeyLeft:
		if c.inputCursor > 0 {
			c.inputCursor--
		}
		c.redrawPrompt()
		return

	case KeyRight:
		if c.inputCursor < len(c.inputBuf) {
			c.inputCursor++
		}
		c.redrawPrompt()
		return

	case KeyHome, KeyCtrlA:
		c.inputCursor = 0
		c.redrawPrompt()
		return

	case KeyEnd, KeyCtrlE:
		c.inputCursor = len(c.inputBuf)
		c.redrawPrompt()
		return

	case KeyCtrlK:
		// Kill to end of line.
		c.inputBuf = c.inputBuf[:c.inputCursor]
		c.redrawPrompt()
		return

	case KeyCtrlU:
		// Kill to beginning of line.
		c.inputBuf = c.inputBuf[c.inputCursor:]
		c.inputCursor = 0
		c.redrawPrompt()
		return

	case KeyCtrlW:
		// Kill previous word.
		if c.inputCursor > 0 {
			end := c.inputCursor
			// Skip trailing spaces.
			for c.inputCursor > 0 && c.inputBuf[c.inputCursor-1] == ' ' {
				c.inputCursor--
			}
			// Skip word chars.
			for c.inputCursor > 0 && c.inputBuf[c.inputCursor-1] != ' ' {
				c.inputCursor--
			}
			c.inputBuf = append(c.inputBuf[:c.inputCursor], c.inputBuf[end:]...)
		}
		c.redrawPrompt()
		return

	case KeyNone:
		// Printable rune.
		if ev.Rune != 0 {
			if c.completer.Active() {
				c.completer.Accept()
			}
			c.inputBuf = append(c.inputBuf[:c.inputCursor],
				append([]rune{ev.Rune}, c.inputBuf[c.inputCursor:]...)...)
			c.inputCursor++
			c.redrawPrompt()
			return
		}
	}
}

func (c *ShellCore) handleSearchKey(ev KeyEvent) {
	switch ev.Key {
	case KeyCtrlC, KeyEsc:
		c.mode = ModeInput
		c.redrawPrompt()
		return

	case KeyCtrlR:
		if c.searchIdx > 0 {
			c.searchIdx--
			if match, idx := c.history.Search(c.searchBuf, c.searchIdx); idx >= 0 {
				c.searchIdx = idx
				c.inputBuf = []rune(match)
				c.inputCursor = len(c.inputBuf)
			}
		}
		c.redrawPrompt()
		return

	case KeyEnter:
		c.mode = ModeInput
		c.history.Reset()
		c.redrawPrompt()
		return

	case KeyBackspace:
		if len(c.searchBuf) > 0 {
			c.searchBuf = c.searchBuf[:len(c.searchBuf)-1]
			c.searchIdx = c.history.Len() - 1
			if match, idx := c.history.Search(c.searchBuf, c.searchIdx); idx >= 0 {
				c.searchIdx = idx
				c.inputBuf = []rune(match)
				c.inputCursor = len(c.inputBuf)
			}
		}
		c.redrawPrompt()
		return

	case KeyNone:
		if ev.Rune != 0 {
			c.searchBuf += string(ev.Rune)
			if match, idx := c.history.Search(c.searchBuf, c.searchIdx); idx >= 0 {
				c.searchIdx = idx
				c.inputBuf = []rune(match)
				c.inputCursor = len(c.inputBuf)
			}
			c.redrawPrompt()
			return
		}
	}
}

// ── Builtins ────────────────────────────────────────────────

// execBuiltin handles synchronous built-in commands.
// Returns true if the line was a builtin (including exit/quit).
func (c *ShellCore) execBuiltin(line string) bool {
	switch {
	case line == "exit" || line == "quit":
		c.quitting = true
		c.dispatch(ActionQuit{})
		return true
	case line == "clear":
		// Erase prompt, then tell renderer to clear screen.
		var b strings.Builder
		c.erasePromptLines(&b)
		b.WriteString("\x1b[2J\x1b[H") // clear screen + home cursor
		c.write([]byte(b.String()))
		c.scrollback.Reset()
		c.resetFlushState()
		return true
	}

	// Help topics and man pages. Page through less when the content
	// wouldn't fit on one screen — the top-level `help` index is
	// deliberately short enough to skip the pager every time.
	if text, ok := c.topicDispatch(line); ok {
		c.showDoc(text, line)
		return true
	}
	switch {
	case line == "history":
		entries := c.history.Entries()
		for i, entry := range entries {
			c.appendScrollback(fmt.Sprintf("%4d  %s\n", i+1, entry))
		}
		return true
	case line == "range":
		c.appendScrollback(formatRange(c.rangeStart, c.rangeEnd))
		return true
	case strings.HasPrefix(line, "range "):
		start, end, err := parseRange(strings.TrimPrefix(line, "range "))
		if err != nil {
			c.appendScrollback(ErrorText(fmt.Sprintf("range: %v", err)) + "\n")
			return true
		}
		c.rangeStart = start
		c.rangeEnd = end
		c.dispatch(ActionSetRange{Start: start, End: end})
		c.appendScrollback(formatRange(start, end))
		return true
	case strings.HasPrefix(line, "vi ") || strings.HasPrefix(line, "vim "):
		parts := strings.Fields(line)
		if len(parts) < 2 {
			c.appendScrollback(ErrorText("usage: vi <path>") + "\n")
			return true
		}
		if c.shellFS == nil {
			c.appendScrollback(ErrorText("vi: no filesystem available") + "\n")
			return true
		}
		c.startVim(parts[1])
		return true
	case line == "cd" || line == "cd ~":
		c.cwd = "/"
		return true
	case strings.HasPrefix(line, "cd "):
		target := strings.TrimSpace(strings.TrimPrefix(line, "cd "))
		if target == "~" || target == "" {
			c.cwd = "/"
			return true
		}
		resolved := c.resolvePath(target)
		if c.shellFS != nil {
			if _, err := c.shellFS.ReadDir(resolved); err != nil {
				c.appendScrollback(ErrorText(fmt.Sprintf("cd: %s: no such directory", target)) + "\n")
				return true
			}
		}
		c.cwd = resolved
		return true
	}

	// FS-aware builtins: ls, cat, rm, which, type for /bin /cron.d /etc /log paths.
	if handled := c.execFSBuiltin(line); handled {
		return true
	}

	// Chart builtin — `chart <watch>` reads recent watch results and renders.
	if parts := strings.Fields(line); len(parts) > 0 && parts[0] == "chart" {
		if c.execChart(parts) {
			return true
		}
	}

	return false
}

// resolvePath resolves a path against the current working directory.
// Absolute paths pass through; relative paths are joined with cwd.
// ".." segments are normalized.
func (c *ShellCore) resolvePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		if c.cwd == "/" {
			path = "/" + path
		} else {
			path = c.cwd + "/" + path
		}
	}
	// Normalize: split on /, resolve . and .., rejoin.
	parts := strings.Split(path, "/")
	var resolved []string
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
		default:
			resolved = append(resolved, p)
		}
	}
	if len(resolved) == 0 {
		return "/"
	}
	return "/" + strings.Join(resolved, "/")
}

// resolveQueryPaths rewrites a command's arguments, resolving any that
// refer to query-source paths against the current working directory.
// e.g. from cwd=/log/weft: ["tail", "api.log"] → "tail /log/weft/api.log"
func (c *ShellCore) resolveQueryPaths(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	resolved := make([]string, len(parts))
	resolved[0] = parts[0] // command name
	for i := 1; i < len(parts); i++ {
		arg := parts[i]
		if strings.HasPrefix(arg, "-") {
			resolved[i] = arg
			continue
		}
		path := c.resolvePath(arg)
		if ClassifyPath(path) == PathQuery {
			resolved[i] = path
		} else {
			resolved[i] = arg
		}
	}
	return strings.Join(resolved, " ")
}

// normalizePath resolves relative paths against root.
// "log" → "/log", "log/vimgym.log" → "/log/vimgym.log", "/bin" → "/bin"
func normalizePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// execFSBuiltin handles commands that interact with the virtual filesystem.
func (c *ShellCore) execFSBuiltin(line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}
	cmd := parts[0]
	args := parts[1:]

	// Detect "cat > /path" or "cat - > /path" — stdin capture mode.
	if cmd == "cat" {
		redirectIdx := -1
		for i, a := range args {
			if a == ">" {
				redirectIdx = i
				break
			}
		}
		if redirectIdx >= 0 && redirectIdx+1 < len(args) {
			target := c.resolvePath(args[redirectIdx+1])
			if ClassifyPath(target) == PathLocal {
				if c.shellFS == nil {
					c.appendScrollback(ErrorText("cat: filesystem not available") + "\n")
					return true
				}
				c.startCapture(target)
				return true
			}
		}
	}

	// These commands are always builtins — never fall through to the query pipeline.
	switch cmd {
	case "ls", "rm", "which", "type":
		// handled below
	case "cat":
		// Only intercept if the target resolves to a known FS path.
		if len(args) > 0 {
			path := c.resolvePath(args[0])
			if ClassifyPath(path) == PathUnknown {
				return false
			}
		} else {
			return false // bare "cat" with no args
		}
	case "head", "tail":
		// Only intercept if targeting a query source.
		hasQueryPath := false
		for _, a := range args {
			if ClassifyPath(c.resolvePath(a)) == PathQuery {
				hasQueryPath = true
				break
			}
		}
		if !hasQueryPath {
			return false
		}
	default:
		return false
	}

	if c.shellFS == nil {
		c.appendScrollback(ErrorText(cmd+": filesystem not available") + "\n")
		return true
	}

	switch cmd {
	case "ls":
		longForm := false
		path := c.cwd
		for _, a := range args {
			if a == "-l" || a == "-al" || a == "-la" {
				longForm = true
				continue
			}
			if !strings.HasPrefix(a, "-") {
				path = c.resolvePath(a)
			}
		}
		entries, err := c.shellFS.ReadDir(path)
		if err != nil {
			c.appendScrollback(ErrorText(fmt.Sprintf("ls: %s: no such directory", path)) + "\n")
			return true
		}
		// Long-form for /cron.d/ adds a per-watch sparkline glyph beside
		// each entry. Falls back silently if the FS doesn't expose
		// WatchResultsProvider.
		if longForm && (path == "/cron.d" || path == "/cron.d/") {
			c.lsCronWatchesLong(entries)
			return true
		}
		for _, name := range entries {
			c.appendScrollback(name + "\n")
		}
		return true

	case "cat":
		if len(args) == 0 {
			return false
		}
		path := c.resolvePath(args[0])

		// Query sources — pass to coordinator with resolved path.
		if ClassifyPath(path) == PathQuery {
			resolved := cmd + " " + path
			if len(parts) > 2 {
				resolved += " " + strings.Join(parts[2:], " ")
			}
			c.startQuery(resolved)
			c.redrawPrompt()
			return true
		}

		// Local FS paths — read from FS.
		data, err := c.shellFS.ReadFile(path)
		if err != nil {
			c.appendScrollback(ErrorText(fmt.Sprintf("cat: %s: no such file", path)) + "\n")
			return true
		}
		content := string(data)
		if content != "" {
			c.appendScrollback(content)
			if !strings.HasSuffix(content, "\n") {
				c.appendScrollback("\n")
			}
		}
		return true

	case "rm":
		if len(args) == 0 {
			c.appendScrollback(ErrorText("rm: missing operand") + "\n")
			return true
		}
		path := c.resolvePath(args[0])
		if err := c.shellFS.Remove(path); err != nil {
			c.appendScrollback(ErrorText(fmt.Sprintf("rm: %v", err)) + "\n")
			return true
		}
		return true

	case "head", "tail":
		// Query source commands — resolve paths against cwd before sending.
		resolved := c.resolveQueryPaths(parts)
		c.startQuery(resolved)
		c.redrawPrompt()
		return true

	case "which":
		if len(args) == 0 {
			return false
		}
		name := args[0]
		if _, err := c.shellFS.ReadFile("/bin/" + name); err == nil {
			c.appendScrollback(fmt.Sprintf("/bin/%s\n", name))
			return true
		}
		c.appendScrollback(ErrorText(fmt.Sprintf("which: %s not found", name)) + "\n")
		return true

	case "type":
		if len(args) == 0 {
			return false
		}
		name := args[0]
		for _, b := range []string{"exit", "quit", "clear", "history", "range", "cd", "vi", "vim", "ls", "cat", "rm", "which", "type"} {
			if name == b {
				c.appendScrollback(fmt.Sprintf("%s is a shell builtin\n", name))
				return true
			}
		}
		if _, err := c.shellFS.ReadFile("/bin/" + name); err == nil {
			c.appendScrollback(fmt.Sprintf("%s is /bin/%s\n", name, name))
			return true
		}
		c.appendScrollback(ErrorText(fmt.Sprintf("type: %s: not found", name)) + "\n")
		return true
	}

	return false
}

// resolveFunction checks if the command line invokes a /bin/ function.
// If the first word matches a file in /bin/, returns line unchanged so the
// invocation (name + args) flows to the server verbatim — the coordinator
// pre-expands bash bodies and warp resolves /bin directly for both bash
// and lua. Returns "" if no matching /bin entry exists.
// warpBuiltins are commands that warp handles natively — don't look these up in /bin/.
var warpBuiltins = map[string]bool{
	"echo": true, "printf": true, "read": true, "test": true,
	"true": true, "false": true, "set": true, "export": true,
	"local": true, "declare": true, "let": true, "eval": true,
	"seq": true, "yes": true, "rev": true, "tac": true,
	"sort": true, "uniq": true, "wc": true, "tr": true,
	"cut": true, "paste": true, "tee": true, "xargs": true,
	"grep": true, "awk": true, "sed": true, "jq": true,
	"alert": true,
	"weft.latest": true, "weft.avg": true, "weft.max": true,
	"weft.min": true, "weft.sum": true, "weft.stddev": true,
}

func (c *ShellCore) resolveFunction(line string) string {
	if c.shellFS == nil {
		return ""
	}
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return ""
	}
	name := parts[0]

	// Don't look up warp builtins/commands in /bin/.
	if warpBuiltins[name] {
		return ""
	}

	// Try the name as-is if it's a /bin/ path, or prepend /bin/.
	path := normalizePath(name)
	if strings.HasPrefix(path, "/bin/") {
		// "/bin/check_errors" or "bin/check_errors" → read directly
	} else {
		// "check_errors" → try /bin/check_errors
		path = "/bin/" + name
	}

	if _, err := c.shellFS.ReadFile(path); err != nil {
		return ""
	}

	// Both bash and lua invocations flow through unchanged. The coordinator
	// pre-expands bash bodies (for planner visibility into `cat /log/...`)
	// and warp's PATH resolution handles execution for both languages.
	return line
}

// ── Query handling ──────────────────────────────────────────

func (c *ShellCore) startQuery(line string) {
	weftPipeline, termCmd := splitTerminalStage(line)
	c.pendingLess = termCmd == "less"

	if isFollowPipeline(weftPipeline) {
		c.mode = ModeFollowing
		c.appendScrollback(DimText("[streaming — Ctrl+C to stop]") + "\n")
		c.dispatch(ActionFollow{Pipeline: weftPipeline})
		return
	}

	c.mode = ModeRunning
	c.spinnerFrame = 0
	c.dispatch(ActionQuery{Pipeline: weftPipeline})
}

func (c *ShellCore) handleQueryResult(ev QueryResultEvent) {
	c.mode = ModeInput

	if ev.Err != nil {
		c.pendingLess = false
		errMsg := cleanError(ev.Err.Error())
		c.appendScrollback(ErrorText(errMsg) + "\n")
		c.redrawPrompt()
		return
	}

	if c.pendingLess {
		c.pendingLess = false
		text := ev.Text
		if text == "" && len(ev.Rows) > 0 {
			var sb strings.Builder
			for _, row := range ev.Rows {
				sb.WriteString(row.Raw)
				sb.WriteByte('\n')
			}
			text = sb.String()
		}
		c.enterLess(text, "")
		return
	}

	if ev.Text != "" {
		c.appendScrollback(ev.Text)
	} else {
		for _, row := range ev.Rows {
			c.appendScrollback(row.Raw + "\n")
		}
	}
	c.redrawPrompt()
}

func (c *ShellCore) handleFollowRow(ev FollowRowEvent) {
	if c.mode == ModeFollowing {
		c.appendScrollback(ev.Raw + "\n")
	}
}

func (c *ShellCore) handleFollowDone(ev FollowDoneEvent) {
	if c.mode == ModeFollowing || c.mode == ModeInput {
		c.mode = ModeInput
		if ev.Err != nil {
			c.appendScrollback(ErrorText(ev.Err.Error()) + "\n")
		}
		c.appendScrollback(DimText("[tail stopped]") + "\n")
		c.redrawPrompt()
	}
}

// cleanError strips noisy gRPC/transport wrappers from error messages
// to produce something a user can read.
func cleanError(msg string) string {
	// Strip gRPC prefixes.
	for _, prefix := range []string{
		"rpc error: code = Unknown desc = ",
		"rpc error: code = Internal desc = ",
		"compute error: ",
	} {
		if idx := strings.Index(msg, prefix); idx >= 0 {
			msg = msg[idx+len(prefix):]
		}
	}

	// "connection refused" buried in transport noise → short message.
	if strings.Contains(msg, "connection refused") {
		return "coordinator unavailable (connection refused)"
	}
	if strings.Contains(msg, "Unavailable") && strings.Contains(msg, "transport") {
		return "coordinator unavailable"
	}

	// Strip wrapping quotes from gRPC desc fields.
	msg = strings.TrimPrefix(msg, "\"")
	msg = strings.TrimSuffix(msg, "\"")

	return msg
}

// ── Pipeline helpers ────────────────────────────────────────

// isFollowPipeline checks if the pipeline is a tail -f command.
func isFollowPipeline(pipeline string) bool {
	parts := strings.Fields(pipeline)
	if len(parts) < 2 || parts[0] != "tail" {
		return false
	}
	for _, p := range parts {
		if p == "-f" {
			return true
		}
	}
	return false
}

// splitTerminalStage checks if the pipeline ends with a terminal command
// (less, more) and splits it off. The termPart is the bare command name
// ("less" or "more") rather than the whole last-stage string — the
// caller doesn't need flags today and stripping them avoids confusing
// future pager invocations with stray argv.
func splitTerminalStage(pipeline string) (weftPart, termPart string) {
	idx := strings.LastIndex(pipeline, "|")
	if idx == -1 {
		return pipeline, ""
	}
	lastStage := strings.TrimSpace(pipeline[idx+1:])
	cmd := strings.Fields(lastStage)
	if len(cmd) > 0 && terminalCommands[cmd[0]] {
		return strings.TrimSpace(pipeline[:idx]), cmd[0]
	}
	return pipeline, ""
}

// SplitTerminalStage is the exported form for non-interactive callers
// (e.g. `weft sh -c 'cat foo.log | less'`) that want to strip a pager
// suffix before sending the pipeline to the coordinator.
func SplitTerminalStage(pipeline string) (weftPart, termPart string) {
	return splitTerminalStage(pipeline)
}

// ── Output helpers ──────────────────────────────────────────

func (c *ShellCore) appendScrollback(s string) {
	// Use \r\n so xterm.js returns to column 0 on each newline.
	s = strings.ReplaceAll(s, "\r\n", "\n")    // normalize first
	s = strings.ReplaceAll(s, "\n", "\r\n")
	c.scrollback.WriteString(s)
	c.flushScrollback()
}

// flushScrollback sends only NEW scrollback content since the last flush.
// Erases the prompt, writes new content, then the caller redraws the prompt.
func (c *ShellCore) flushScrollback() {
	full := c.scrollback.String()
	newContent := full[c.lastFlushedLen:]
	c.lastFlushedLen = len(full)
	if len(newContent) == 0 {
		return
	}

	var b strings.Builder
	c.erasePromptLines(&b)
	b.WriteString(newContent)
	c.write([]byte(b.String()))
	c.prevPromptLines = 0
}

// redrawPrompt erases the previous prompt and writes the new one.
func (c *ShellCore) redrawPrompt() {
	var b strings.Builder

	c.erasePromptLines(&b)

	info := c.promptInfo()

	var prompt strings.Builder
	switch c.mode {
	case ModeVim:
		return // vim handles its own rendering
	case ModeCapture:
		return // capture mode renders its own prompt
	case ModeRunning:
		prompt.WriteString(RenderRunningPrompt(c.spinnerFrame))
	case ModeSearching:
		prompt.WriteString(RenderPrompt(ModeSearching, c.searchBuf, c.GetInput(), c.inputCursor, info))
	default:
		prompt.WriteString(RenderPrompt(ModeInput, "", c.GetInput(), c.inputCursor, info))
	}

	promptStr := prompt.String()
	b.WriteString(promptStr)

	lines := 1
	for _, ch := range promptStr {
		if ch == '\n' {
			lines++
		}
	}
	c.prevPromptLines = lines

	// Position the terminal cursor at the correct column for input modes.
	if c.mode == ModeInput || c.mode == ModeSearching {
		// If the completer menu added lines below, move cursor back up.
		menuLines := lines - 1
		if menuLines > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", menuLines)
		}
		// Absolute column positioning: prompt prefix width + cursor offset + 1 (1-indexed).
		col := PromptPrefixWidth(c.mode, c.searchBuf, info) + c.inputCursor + 1
		fmt.Fprintf(&b, "\x1b[%dG", col)
	}

	c.write([]byte(b.String()))
}

// erasePromptLines writes ANSI sequences to erase the previous prompt area.
func (c *ShellCore) erasePromptLines(b *strings.Builder) {
	if c.prevPromptLines <= 0 {
		return
	}
	b.WriteString("\r")
	for i := 1; i < c.prevPromptLines; i++ {
		b.WriteString("\x1b[A")
	}
	b.WriteString("\x1b[J")
}

// resetFlushState resets the incremental flush tracking.
func (c *ShellCore) resetFlushState() {
	c.lastFlushedLen = 0
	c.prevPromptLines = 0
}
