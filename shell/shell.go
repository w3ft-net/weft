package shell

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kevlarcon/weft/shard"
)

var (
	promptStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Inline(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Inline(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Inline(true)
	searchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Inline(true)
	borderColor  = lipgloss.Color("68")
	accentStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Inline(true)
	sepStyle     = lipgloss.NewStyle().Foreground(borderColor)
)

// terminalCommands are commands that need direct terminal access (pagers, etc.).
var terminalCommands = map[string]bool{
	"less": true,
	"more": true,
}

// QueryResult is the output of a query — either structured rows or text.
type QueryResult struct {
	Rows []shard.LogRow
	Text string
}

// QueryFunc is the function signature the shell uses to execute queries.
// Abstracts both in-process coordinator and remote gRPC coordinator.
type QueryFunc func(pipeline string, start, end time.Time) (*QueryResult, error)

// Shell is the weft interactive shell.
type Shell struct {
	Query QueryFunc
	Start time.Time
	End   time.Time
	Apps  []string
}

// Run starts the interactive bubbletea REPL in full-screen mode.
func (s *Shell) Run() error {
	m := newModel(s)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// history tracks command history with cursor navigation.
type history struct {
	entries []string
	cursor  int    // -1 = not navigating; 0..len-1 = browsing history
	saved   string // the in-progress input before the user started navigating
}

func (h *history) add(line string) {
	if line == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		return
	}
	h.entries = append(h.entries, line)
	h.cursor = -1
}

func (h *history) up(currentInput string) (string, bool) {
	if len(h.entries) == 0 {
		return "", false
	}
	if h.cursor == -1 {
		h.saved = currentInput
		h.cursor = len(h.entries) - 1
	} else if h.cursor > 0 {
		h.cursor--
	} else {
		return h.entries[0], false
	}
	return h.entries[h.cursor], true
}

func (h *history) down(currentInput string) (string, bool) {
	if h.cursor == -1 {
		return "", false
	}
	if h.cursor < len(h.entries)-1 {
		h.cursor++
		return h.entries[h.cursor], true
	}
	h.cursor = -1
	return h.saved, true
}

func (h *history) reset() {
	h.cursor = -1
}

func (h *history) search(query string, fromIdx int) (string, int) {
	if query == "" {
		return "", -1
	}
	start := fromIdx
	if start < 0 || start >= len(h.entries) {
		start = len(h.entries) - 1
	}
	for i := start; i >= 0; i-- {
		if strings.Contains(h.entries[i], query) {
			return h.entries[i], i
		}
	}
	return "", -1
}

// Spinner frames — braille dot cycle.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var spinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Inline(true)

// Messages for async query execution.
type spinnerTickMsg time.Time

type queryResultMsg struct {
	result  *QueryResult
	err     error
	termCmd string
}

func spinnerTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}

// headerLines is the fixed height of the header (logo rows).
const headerLines = 5

// model is the bubbletea model for the shell.
type model struct {
	shell      *Shell
	input      textinput.Model
	viewport   viewport.Model
	scrollback *strings.Builder // accumulated output fed into viewport
	history    *history
	completer  *completer
	quitting   bool
	width      int
	height     int
	ready      bool // true after first WindowSizeMsg

	// Reverse search state.
	searching bool
	searchBuf string
	searchIdx int

	// Async query state.
	running      bool
	spinnerFrame int
}

func newModel(s *Shell) model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Focus()
	ti.CharLimit = 1024

	return model{
		shell:      s,
		input:      ti,
		scrollback: &strings.Builder{},
		history:    &history{},
		completer:  newCompleter(s.Apps),
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

// viewportHeight calculates the available rows for the scrollback viewport.
func (m model) viewportHeight() int {
	// Layout: border(1) + header(5) + info-bar(1) + sep(1) + viewport + sep(1) + input(1) + completer + border(1)
	chrome := 2 + headerLines + 1 + 1 + 1 + 1 // borders + header + info bar + separators + input
	if m.completer.active {
		lines := len(m.completer.items)
		if lines > 8 {
			lines = 9 // 8 visible + "... N more"
		}
		chrome += lines
	}
	h := m.height - chrome
	if h < 1 {
		h = 1
	}
	return h
}

// innerWidth returns the usable width inside the border.
func (m model) innerWidth() int {
	w := m.width - 2 // left + right border
	if w < 20 {
		w = 20
	}
	return w
}

// syncViewport updates the viewport content and dimensions.
func (m *model) syncViewport() {
	m.viewport.Width = m.innerWidth()
	m.viewport.Height = m.viewportHeight()
	m.viewport.SetContent(m.scrollback.String())
	m.viewport.GotoBottom()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(m.innerWidth(), m.viewportHeight())
			m.viewport.SetContent(m.scrollback.String())
			m.ready = true
		} else {
			m.viewport.Width = m.innerWidth()
			m.viewport.Height = m.viewportHeight()
		}
		return m, nil

	case spinnerTickMsg:
		if m.running {
			m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
			return m, spinnerTick()
		}
		return m, nil

	case queryResultMsg:
		m.running = false
		m.handleQueryResult(msg)
		m.syncViewport()
		if m.quitting {
			return m, tea.Quit
		}
		return m, nil

	case tea.MouseMsg:
		if m.ready {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyMsg:
		// Ignore input while a query is running.
		if m.running {
			if msg.Type == tea.KeyCtrlC {
				m.running = false
				m.scrollback.WriteString(errorStyle.Render("interrupted") + "\n")
				m.syncViewport()
				return m, nil
			}
			return m, nil
		}

		// Reverse search mode.
		if m.searching {
			return m.updateSearch(msg)
		}

		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyCtrlD:
			m.completer.dismiss()
			m.quitting = true
			return m, tea.Quit

		case tea.KeyCtrlR:
			m.completer.dismiss()
			m.searching = true
			m.searchBuf = ""
			m.searchIdx = len(m.history.entries) - 1
			return m, nil

		case tea.KeyPgUp:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd

		case tea.KeyPgDown:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd

		case tea.KeyTab:
			if m.completer.active {
				m.completer.next()
				newInput, newCursor := m.completer.apply(m.input.Value())
				m.input.SetValue(newInput)
				m.input.SetCursor(newCursor)
			} else {
				m.completer.update(m.input.Value(), m.input.Position())
				if m.completer.active {
					newInput, newCursor := m.completer.apply(m.input.Value())
					m.input.SetValue(newInput)
					m.input.SetCursor(newCursor)
				}
			}
			return m, nil

		case tea.KeyShiftTab:
			if m.completer.active {
				m.completer.prev()
				newInput, newCursor := m.completer.apply(m.input.Value())
				m.input.SetValue(newInput)
				m.input.SetCursor(newCursor)
			}
			return m, nil

		case tea.KeyEsc:
			if m.completer.active {
				m.completer.dismiss()
				return m, nil
			}

		case tea.KeyUp:
			m.completer.dismiss()
			if line, ok := m.history.up(m.input.Value()); ok {
				m.input.SetValue(line)
				m.input.CursorEnd()
			}
			return m, nil

		case tea.KeyDown:
			m.completer.dismiss()
			if line, ok := m.history.down(m.input.Value()); ok {
				m.input.SetValue(line)
				m.input.CursorEnd()
			}
			return m, nil

		case tea.KeyEnter:
			m.completer.dismiss()
			line := strings.TrimSpace(m.input.Value())
			m.input.SetValue("")
			m.history.reset()
			if line == "" {
				return m, nil
			}
			m.history.add(line)
			m.scrollback.WriteString(promptStyle.Render("weft> ") + highlightPipeline(line) + "\n")

			if cmd := m.execBuiltin(line); cmd != nil || m.quitting {
				m.syncViewport()
				if m.quitting {
					return m, tea.Quit
				}
				return m, cmd
			}

			m.syncViewport()
			return m, m.startQuery(line)

		default:
			if m.completer.active {
				m.completer.dismiss()
			}
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		m.searching = false
		return m, nil

	case tea.KeyCtrlR:
		if m.searchIdx > 0 {
			m.searchIdx--
			if match, idx := m.history.search(m.searchBuf, m.searchIdx); idx >= 0 {
				m.searchIdx = idx
				m.input.SetValue(match)
				m.input.CursorEnd()
			}
		}
		return m, nil

	case tea.KeyEnter:
		m.searching = false
		m.history.reset()
		return m, nil

	case tea.KeyBackspace:
		if len(m.searchBuf) > 0 {
			m.searchBuf = m.searchBuf[:len(m.searchBuf)-1]
			m.searchIdx = len(m.history.entries) - 1
			if match, idx := m.history.search(m.searchBuf, m.searchIdx); idx >= 0 {
				m.searchIdx = idx
				m.input.SetValue(match)
				m.input.CursorEnd()
			}
		}
		return m, nil

	default:
		if msg.Type == tea.KeyRunes {
			m.searchBuf += string(msg.Runes)
			if match, idx := m.history.search(m.searchBuf, m.searchIdx); idx >= 0 {
				m.searchIdx = idx
				m.input.SetValue(match)
				m.input.CursorEnd()
			}
			return m, nil
		}
	}
	return m, nil
}

// execBuiltin handles synchronous built-in commands. Returns nil if the
// line is not a built-in (i.e. it's a pipeline that should run async).
func (m *model) execBuiltin(line string) tea.Cmd {
	switch {
	case line == "exit" || line == "quit":
		m.quitting = true
		return nil
	case line == "apps":
		m.scrollback.WriteString(dimStyle.Render("Available apps:\n"))
		for _, app := range m.shell.Apps {
			m.scrollback.WriteString(fmt.Sprintf("  %s.log\n", app))
		}
		m.scrollback.WriteString(fmt.Sprintf("  warp.log %s\n", dimStyle.Render("(all apps)")))
		return tea.Batch()
	case line == "range":
		m.scrollback.WriteString(fmt.Sprintf("  %s to %s\n",
			m.shell.Start.Format("2006-01-02T15:04:05Z"),
			m.shell.End.Format("2006-01-02T15:04:05Z")))
		return tea.Batch()
	case strings.HasPrefix(line, "range "):
		m.scrollback.WriteString(dimStyle.Render("range adjustment not yet implemented\n"))
		return tea.Batch()
	}
	return nil
}

// startQuery launches an async pipeline query and starts the spinner.
func (m *model) startQuery(line string) tea.Cmd {
	m.running = true
	m.spinnerFrame = 0

	weftPipeline, termCmd := splitTerminalStage(line)
	queryFn := m.shell.Query
	start, end := m.shell.Start, m.shell.End

	return tea.Batch(spinnerTick(), func() tea.Msg {
		result, err := queryFn(weftPipeline, start, end)
		return queryResultMsg{result: result, err: err, termCmd: termCmd}
	})
}

// handleQueryResult processes the result of an async query.
func (m *model) handleQueryResult(msg queryResultMsg) {
	if msg.err != nil {
		errMsg := msg.err.Error()
		for _, prefix := range []string{
			"rpc error: code = Unknown desc = ",
			"compute error: ",
		} {
			if idx := strings.Index(errMsg, prefix); idx >= 0 {
				errMsg = errMsg[idx+len(prefix):]
			}
		}
		m.scrollback.WriteString(errorStyle.Render(errMsg) + "\n")
		return
	}

	var text string
	if msg.result.Text != "" {
		text = msg.result.Text
	} else {
		var sb strings.Builder
		for _, row := range msg.result.Rows {
			sb.WriteString(row.Raw)
			sb.WriteByte('\n')
		}
		text = sb.String()
	}

	if msg.termCmd != "" {
		m.scrollback.WriteString(dimStyle.Render(fmt.Sprintf("[piping to %s]\n", msg.termCmd)))
		pipeToTerminal(text, msg.termCmd)
	} else {
		m.scrollback.WriteString(text)
	}
}

// ── View ─────────────────────────────────────────────────────────

func (m model) View() string {
	if !m.ready {
		return ""
	}
	if m.quitting {
		return "" // alt-screen will be cleared on exit
	}

	w := m.innerWidth()

	// ── Header: logo (left) + info panel (right) ──
	header := m.renderHeader(w)

	// ── Separator ──
	sep := sepStyle.Render(strings.Repeat("─", w))

	// ── Viewport (scrollback) ──
	// Recalculate height in case completer changed.
	m.viewport.Height = m.viewportHeight()
	vpView := m.viewport.View()

	// ── Input line ──
	var inputLine string
	if m.running {
		frame := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
		inputLine = " " + spinnerStyle.Render(frame) + dimStyle.Render(" querying shards...")
	} else if m.searching {
		inputLine = " " + searchStyle.Render(fmt.Sprintf("(reverse-i-search)`%s': ", m.searchBuf)) +
			highlightPipeline(m.input.Value())
	} else {
		inputLine = " " + promptStyle.Render("weft> ") +
			highlightWithCursor(m.input.Value(), m.input.Position())
	}

	// ── Completer (between separator and input) ──
	var completerView string
	if m.completer.active {
		completerView = m.completer.render()
	}

	// ── Compose inner content ──
	var sections []string
	sections = append(sections, header)
	sections = append(sections, sep)
	sections = append(sections, vpView)
	sections = append(sections, sep)
	if completerView != "" {
		sections = append(sections, completerView)
	}
	sections = append(sections, inputLine)

	inner := strings.Join(sections, "\n")

	// ── Wrap in border ──
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(w)

	return boxStyle.Render(inner)
}

// renderHeader builds the logo + info panel side by side.
func (m model) renderHeader(totalWidth int) string {
	logo := RenderLogo()
	logoLines := strings.Split(strings.TrimRight(logo, "\n"), "\n")

	// Info panel lines — matched to logo height.
	rangeStr := fmt.Sprintf("%s to %s",
		m.shell.Start.Format("15:04"),
		m.shell.End.Format("15:04"))

	appsStr := fmt.Sprintf("%d apps", len(m.shell.Apps))
	if len(m.shell.Apps) <= 4 {
		names := make([]string, len(m.shell.Apps))
		copy(names, m.shell.Apps)
		appsStr = strings.Join(names, ", ")
	}

	infoLines := []string{
		accentStyle.Render(rangeStr),
		dimStyle.Render(appsStr),
		"",
		dimStyle.Render("apps") + " " + dimStyle.Render("range") + " " + dimStyle.Render("exit"),
		dimStyle.Render("ctrl-r") + " " + dimStyle.Render("ctrl-c"),
	}

	// Pad to same number of lines.
	for len(logoLines) < len(infoLines) {
		logoLines = append(logoLines, "")
	}
	for len(infoLines) < len(logoLines) {
		infoLines = append(infoLines, "")
	}

	// Measure the visual width of the logo (first line as reference).
	logoWidth := visualWidth(logoLines[0])
	if logoWidth < 53 {
		logoWidth = 53 // known width from letter bitmaps
	}

	gap := 4
	infoWidth := totalWidth - logoWidth - gap
	if infoWidth < 10 {
		infoWidth = 10
	}

	// Build combined header lines.
	var headerBuf strings.Builder
	for i := 0; i < len(logoLines); i++ {
		ll := logoLines[i]
		il := ""
		if i < len(infoLines) {
			il = infoLines[i]
		}
		// Pad logo line to fixed width.
		pad := logoWidth - visualWidth(ll)
		if pad < 0 {
			pad = 0
		}
		headerBuf.WriteString(ll)
		headerBuf.WriteString(strings.Repeat(" ", pad+gap))
		headerBuf.WriteString(il)
		if i < len(logoLines)-1 {
			headerBuf.WriteByte('\n')
		}
	}

	return headerBuf.String()
}

// visualWidth returns the display width of a string, ignoring ANSI escape codes.
func visualWidth(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}

// splitTerminalStage checks if the pipeline ends with a terminal command
// (less, more) and splits it off.
func splitTerminalStage(pipeline string) (weftPart, termPart string) {
	idx := strings.LastIndex(pipeline, "|")
	if idx == -1 {
		return pipeline, ""
	}
	lastStage := strings.TrimSpace(pipeline[idx+1:])
	cmd := strings.Fields(lastStage)
	if len(cmd) > 0 && terminalCommands[cmd[0]] {
		return strings.TrimSpace(pipeline[:idx]), lastStage
	}
	return pipeline, ""
}

// pipeToTerminal runs a terminal command with the given input text.
func pipeToTerminal(text, cmdStr string) {
	parts := strings.Fields(cmdStr)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "pager error: %v\n", err)
	}
}

// ExecNonInteractive runs a single pipeline and prints results to w.
func (s *Shell) ExecNonInteractive(line string, w io.Writer) error {
	weftPipeline, _ := splitTerminalStage(line)
	result, err := s.Query(weftPipeline, s.Start, s.End)
	if err != nil {
		return err
	}

	if result.Text != "" {
		fmt.Fprint(w, result.Text)
	} else {
		for _, row := range result.Rows {
			fmt.Fprintln(w, row.Raw)
		}
	}
	return nil
}
