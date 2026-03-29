package shell

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kevlarcon/weft/coordinator"
)

var (
	promptStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Inline(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Inline(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Inline(true)
	searchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Inline(true)
)

// terminalCommands are commands that need direct terminal access (pagers, etc.).
var terminalCommands = map[string]bool{
	"less": true,
	"more": true,
}

// Shell is the weft interactive shell.
type Shell struct {
	Coord     *coordinator.Coordinator
	TimeRange coordinator.TimeRange
	Apps      []string
}

// Run starts the interactive bubbletea REPL.
func (s *Shell) Run() error {
	m := newModel(s)
	p := tea.NewProgram(m)
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
	// Don't add duplicates of the most recent entry.
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
		// Starting to navigate — save current input.
		h.saved = currentInput
		h.cursor = len(h.entries) - 1
	} else if h.cursor > 0 {
		h.cursor--
	} else {
		return h.entries[0], false // already at oldest
	}
	return h.entries[h.cursor], true
}

func (h *history) down(currentInput string) (string, bool) {
	if h.cursor == -1 {
		return "", false // not navigating
	}
	if h.cursor < len(h.entries)-1 {
		h.cursor++
		return h.entries[h.cursor], true
	}
	// Past the newest entry — restore saved input.
	h.cursor = -1
	return h.saved, true
}

func (h *history) reset() {
	h.cursor = -1
}

// search finds the most recent entry containing the query, starting from
// the current search position. Returns the match and its index, or -1.
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

// model is the bubbletea model for the shell.
type model struct {
	shell     *Shell
	input     textinput.Model
	output    *strings.Builder
	history   *history
	completer *completer
	quitting  bool

	// Reverse search state.
	searching bool
	searchBuf string
	searchIdx int // current search position in history
}

func newModel(s *Shell) model {
	ti := textinput.New()
	ti.Prompt = promptStyle.Render("weft> ")
	ti.Focus()
	ti.CharLimit = 1024

	out := &strings.Builder{}
	out.WriteString(dimStyle.Render(fmt.Sprintf(
		"weft shell — %d apps, range %s to %s",
		len(s.Apps),
		s.TimeRange.Start.Format("15:04"),
		s.TimeRange.End.Format("15:04"),
	)))
	out.WriteString("\n")
	out.WriteString(dimStyle.Render("type a pipeline, or: range, apps, exit"))
	out.WriteString("\n\n")
	return model{
		shell:     s,
		input:     ti,
		output:    out,
		history:   &history{},
		completer: newCompleter(s.Apps),
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
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

		case tea.KeyTab:
			if m.completer.active {
				// Cycle to next match.
				m.completer.next()
				newInput, newCursor := m.completer.apply(m.input.Value())
				m.input.SetValue(newInput)
				m.input.SetCursor(newCursor)
			} else {
				// Open completion menu.
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
			m.output.WriteString(promptStyle.Render("weft> ") + line + "\n")
			m.execLine(line)
			if m.quitting {
				return m, tea.Quit
			}
			return m, nil

		default:
			// Any other key dismisses the completion menu (it will
			// re-evaluate on the next keystroke if needed).
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
		// Cancel search.
		m.searching = false
		return m, nil

	case tea.KeyCtrlR:
		// Search further back.
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
		// Accept the current search result.
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
			} else {
				// No match — keep the search buf but don't change input.
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *model) execLine(line string) {
	switch {
	case line == "exit" || line == "quit":
		m.quitting = true
	case line == "apps":
		m.output.WriteString(dimStyle.Render("Available apps:\n"))
		for _, app := range m.shell.Apps {
			m.output.WriteString(fmt.Sprintf("  %s.log\n", app))
		}
		m.output.WriteString(fmt.Sprintf("  warp.log %s\n", dimStyle.Render("(all apps)")))
	case line == "range":
		m.output.WriteString(fmt.Sprintf("  %s to %s\n",
			m.shell.TimeRange.Start.Format("2006-01-02T15:04:05Z"),
			m.shell.TimeRange.End.Format("2006-01-02T15:04:05Z")))
	case strings.HasPrefix(line, "range "):
		m.output.WriteString(dimStyle.Render("range adjustment not yet implemented\n"))
	default:
		m.execPipeline(line)
	}
}

func (m *model) execPipeline(line string) {
	weftPipeline, termCmd := splitTerminalStage(line)

	result, err := m.shell.Coord.Query(weftPipeline, m.shell.TimeRange)
	if err != nil {
		m.output.WriteString(errorStyle.Render("error: "+err.Error()) + "\n")
		return
	}

	var text string
	if result.Text != "" {
		text = result.Text
	} else {
		var sb strings.Builder
		for _, row := range result.Rows {
			sb.WriteString(row.Raw)
			sb.WriteByte('\n')
		}
		text = sb.String()
	}

	if termCmd != "" {
		m.output.WriteString(dimStyle.Render(fmt.Sprintf("[piping to %s]\n", termCmd)))
		pipeToTerminal(text, termCmd)
	} else {
		m.output.WriteString(text)
	}
}

func (m model) View() string {
	if m.quitting {
		return m.output.String() + dimStyle.Render("goodbye\n")
	}
	if m.searching {
		searchPrompt := searchStyle.Render(fmt.Sprintf("(reverse-i-search)`%s': ", m.searchBuf))
		return m.output.String() + searchPrompt + m.input.Value()
	}
	v := m.output.String() + m.input.View()
	if m.completer.active {
		v += "\n" + m.completer.render()
	}
	return v
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
	result, err := s.Coord.Query(weftPipeline, s.TimeRange)
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
