package shellcore

import (
	"sort"
	"strings"
)

// CompletionKind distinguishes what kind of token we're completing.
type CompletionKind int

const (
	KindCommand CompletionKind = iota
	KindFile
)

// CompletionItem is one possible completion.
type CompletionItem struct {
	Text string
	Kind CompletionKind
}

// Completer manages tab completion state.
type Completer struct {
	Commands []string // available command names
	Files    []string // available pseudofile names

	Active      bool             // completion menu is visible
	Items       []CompletionItem // current matches
	Cursor      int              // selected index in items
	Prefix      string           // the partial word being completed
	PrefixStart int              // byte offset in input where prefix starts
}

// NewCompleter creates a completer with the available apps.
func NewCompleter(apps []string) *Completer {
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

	return &Completer{
		Commands: commands,
		Files:    files,
	}
}

// Update recalculates completions based on the current input and cursor position.
// Returns true if the completion menu should be shown.
func (c *Completer) Update(input string, cursorPos int) bool {
	prefix, start := c.ExtractWord(input, cursorPos)
	if prefix == "" {
		c.Dismiss()
		return false
	}

	c.Prefix = prefix
	c.PrefixStart = start

	kind := c.ClassifyPosition(input, start)

	var candidates []string
	switch kind {
	case KindCommand:
		candidates = c.Commands
	case KindFile:
		candidates = c.Files
	}

	c.Items = nil
	lower := strings.ToLower(prefix)
	for _, cand := range candidates {
		if strings.HasPrefix(strings.ToLower(cand), lower) {
			c.Items = append(c.Items, CompletionItem{Text: cand, Kind: kind})
		}
	}

	if len(c.Items) == 0 {
		c.Dismiss()
		return false
	}

	if len(c.Items) == 1 && strings.EqualFold(c.Items[0].Text, prefix) {
		c.Dismiss()
		return false
	}

	c.Active = true
	c.Cursor = 0
	return true
}

// ClassifyPosition determines whether we're completing a command or a file
// based on what precedes the current word.
func (c *Completer) ClassifyPosition(input string, wordStart int) CompletionKind {
	before := strings.TrimSpace(input[:wordStart])

	if before == "" || strings.HasSuffix(before, "|") {
		return KindCommand
	}

	fields := strings.Fields(before)
	if len(fields) > 0 {
		lastCmd := fields[len(fields)-1]
		if idx := strings.LastIndex(before, "|"); idx >= 0 {
			afterPipe := strings.TrimSpace(before[idx+1:])
			pipeFields := strings.Fields(afterPipe)
			if len(pipeFields) > 0 {
				lastCmd = pipeFields[0]
			}
			if afterPipe == "" {
				return KindCommand
			}
		}
		switch lastCmd {
		case "cat", "head", "tail":
			return KindFile
		}
	}

	return KindCommand
}

// ExtractWord returns the partial word at the cursor and its start position.
func (c *Completer) ExtractWord(input string, cursorPos int) (string, int) {
	if cursorPos > len(input) {
		cursorPos = len(input)
	}
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

// Apply inserts the selected completion into the input string.
// Returns the new input and new cursor position.
func (c *Completer) Apply(input string) (string, int) {
	if !c.Active || len(c.Items) == 0 {
		return input, len(input)
	}
	selected := c.Items[c.Cursor].Text
	newInput := input[:c.PrefixStart] + selected + input[c.PrefixStart+len(c.Prefix):]
	newCursor := c.PrefixStart + len(selected)
	return newInput, newCursor
}

// Next moves to the next completion item.
func (c *Completer) Next() {
	if len(c.Items) == 0 {
		return
	}
	c.Cursor = (c.Cursor + 1) % len(c.Items)
}

// Prev moves to the previous completion item.
func (c *Completer) Prev() {
	if len(c.Items) == 0 {
		return
	}
	c.Cursor = (c.Cursor - 1 + len(c.Items)) % len(c.Items)
}

// Dismiss hides the completion menu.
func (c *Completer) Dismiss() {
	c.Active = false
	c.Items = nil
	c.Cursor = 0
}
