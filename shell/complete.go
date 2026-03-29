package shell

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	matchStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	noMatchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("8")).Foreground(lipgloss.Color("15")).Bold(true)
)

// completionKind distinguishes what kind of token we're completing.
type completionKind int

const (
	kindCommand completionKind = iota
	kindFile
)

// completionItem is one possible completion.
type completionItem struct {
	text string
	kind completionKind
}

// completer manages tab completion state.
type completer struct {
	commands []string // available command names
	files    []string // available pseudofile names

	active  bool             // completion menu is visible
	items   []completionItem // current matches
	cursor  int              // selected index in items
	prefix  string           // the partial word being completed
	prefixStart int          // byte offset in input where prefix starts
}

func newCompleter(apps []string) *completer {
	commands := []string{
		"awk", "cat", "cut", "grep", "head", "jq", "less", "more",
		"paste", "rev", "sed", "seq", "sort", "tac", "tail", "tee",
		"tr", "uniq", "wc", "xargs",
		// weft builtins
		"apps", "exit", "range", "schema",
	}
	sort.Strings(commands)

	var files []string
	for _, app := range apps {
		files = append(files, app+".log")
	}
	files = append(files, "warp.log")
	sort.Strings(files)

	return &completer{
		commands: commands,
		files:    files,
	}
}

// update recalculates completions based on the current input and cursor position.
// Returns true if the completion menu should be shown.
func (c *completer) update(input string, cursorPos int) bool {
	prefix, start := c.extractWord(input, cursorPos)
	if prefix == "" {
		c.dismiss()
		return false
	}

	c.prefix = prefix
	c.prefixStart = start

	// Determine what to complete: if we're right after a pipe or at the
	// start of input, complete commands. Otherwise check if it looks like
	// a file argument (after cat/head/tail).
	kind := c.classifyPosition(input, start)

	var candidates []string
	switch kind {
	case kindCommand:
		candidates = c.commands
	case kindFile:
		candidates = c.files
	}

	c.items = nil
	lower := strings.ToLower(prefix)
	for _, cand := range candidates {
		if strings.HasPrefix(strings.ToLower(cand), lower) {
			c.items = append(c.items, completionItem{text: cand, kind: kind})
		}
	}

	if len(c.items) == 0 {
		c.dismiss()
		return false
	}

	// If the only match is exactly the prefix, don't show the menu.
	if len(c.items) == 1 && strings.EqualFold(c.items[0].text, prefix) {
		c.dismiss()
		return false
	}

	c.active = true
	c.cursor = 0
	return true
}

// classifyPosition determines whether we're completing a command or a file
// based on what precedes the current word.
func (c *completer) classifyPosition(input string, wordStart int) completionKind {
	before := strings.TrimSpace(input[:wordStart])

	// If empty or ends with a pipe, we're at a command position.
	if before == "" || strings.HasSuffix(before, "|") {
		return kindCommand
	}

	// If the preceding word is a source command, complete files.
	fields := strings.Fields(before)
	if len(fields) > 0 {
		lastCmd := fields[len(fields)-1]
		// After a pipe, the command is the first word after the pipe.
		if idx := strings.LastIndex(before, "|"); idx >= 0 {
			afterPipe := strings.TrimSpace(before[idx+1:])
			pipeFields := strings.Fields(afterPipe)
			if len(pipeFields) > 0 {
				lastCmd = pipeFields[0]
			}
			// If afterPipe is empty, we're at a command position.
			if afterPipe == "" {
				return kindCommand
			}
		}
		switch lastCmd {
		case "cat", "head", "tail":
			return kindFile
		}
	}

	return kindCommand
}

// extractWord returns the partial word at the cursor and its start position.
func (c *completer) extractWord(input string, cursorPos int) (string, int) {
	if cursorPos > len(input) {
		cursorPos = len(input)
	}
	// Walk backwards from cursor to find word start.
	start := cursorPos
	for start > 0 {
		ch := input[start-1]
		if ch == ' ' || ch == '|' || ch == '\t' {
			break
		}
		start--
	}
	return input[start:cursorPos], start
}

// apply inserts the selected completion into the input string.
// Returns the new input and new cursor position.
func (c *completer) apply(input string) (string, int) {
	if !c.active || len(c.items) == 0 {
		return input, len(input)
	}
	selected := c.items[c.cursor].text
	newInput := input[:c.prefixStart] + selected + input[c.prefixStart+len(c.prefix):]
	newCursor := c.prefixStart + len(selected)
	return newInput, newCursor
}

// next moves to the next completion item.
func (c *completer) next() {
	if len(c.items) == 0 {
		return
	}
	c.cursor = (c.cursor + 1) % len(c.items)
}

// prev moves to the previous completion item.
func (c *completer) prev() {
	if len(c.items) == 0 {
		return
	}
	c.cursor = (c.cursor - 1 + len(c.items)) % len(c.items)
}

func (c *completer) dismiss() {
	c.active = false
	c.items = nil
	c.cursor = 0
}

// render returns the visual completion menu as a string to display below
// the input line.
func (c *completer) render() string {
	if !c.active || len(c.items) == 0 {
		return ""
	}

	// Show at most 8 items, centered around the cursor.
	maxVisible := 8
	start := 0
	if len(c.items) > maxVisible {
		start = c.cursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		if start+maxVisible > len(c.items) {
			start = len(c.items) - maxVisible
		}
	}
	end := start + maxVisible
	if end > len(c.items) {
		end = len(c.items)
	}

	var sb strings.Builder
	prefixLen := len(c.prefix)

	for i := start; i < end; i++ {
		item := c.items[i]

		if i == c.cursor {
			// Selected item: full highlight.
			sb.WriteString("  ")
			sb.WriteString(selectedStyle.Render(item.text))
		} else {
			// Non-selected: highlight the matching prefix portion.
			sb.WriteString("  ")
			if prefixLen <= len(item.text) {
				sb.WriteString(matchStyle.Render(item.text[:prefixLen]))
				sb.WriteString(noMatchStyle.Render(item.text[prefixLen:]))
			} else {
				sb.WriteString(noMatchStyle.Render(item.text))
			}
		}

		// Show kind indicator for files.
		if item.kind == kindFile {
			sb.WriteString(dimStyle.Render(" (file)"))
		}

		sb.WriteByte('\n')
	}

	if len(c.items) > maxVisible {
		sb.WriteString(dimStyle.Render(fmt.Sprintf("  ... %d more", len(c.items)-maxVisible)))
		sb.WriteByte('\n')
	}

	return sb.String()
}
