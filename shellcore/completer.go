package shellcore

import (
	"path"
	"sort"
	"strings"
)

// builtinCommands are the commands available for tab completion.
var builtinCommands = []string{
	"awk", "cat", "cd", "cut", "grep", "head", "jq", "less", "ls",
	"paste", "rev", "rm", "sed", "seq", "sort", "tac", "tail", "tee",
	"tr", "uniq", "vi", "wc", "which", "xargs",
	// weft builtins
	"apps", "clear", "exit", "help", "man", "range",
}

// Completer manages inline bash-style tab completion.
// First Tab computes matches and inserts the first; subsequent Tabs cycle.
// Any non-Tab key accepts the current completion and resets state.
type Completer struct {
	active       bool
	matches      []string
	cycleIndex   int
	originalWord string // user's text before any completion
	wordStart    int    // rune offset where the word begins
	insertedLen  int    // rune length of what's currently in the buffer at wordStart
}

// NewCompleter creates a completer.
func NewCompleter(_ []string) *Completer {
	return &Completer{}
}

// Active returns whether a completion cycle is in progress.
func (c *Completer) Active() bool { return c.active }

// Accept locks in the current completion and resets cycling state.
func (c *Completer) Accept() {
	c.active = false
	c.matches = nil
	c.cycleIndex = 0
	c.originalWord = ""
	c.wordStart = 0
	c.insertedLen = 0
}

// Tab handles a Tab keypress. Returns the new input and cursor position.
func (c *Completer) Tab(input []rune, cursor int, fs FS, cwd string) ([]rune, int) {
	if c.active {
		c.cycleIndex = (c.cycleIndex + 1) % len(c.matches)
		return c.apply(input)
	}

	word, start := extractWord(input, cursor)
	if word == "" {
		return input, cursor
	}

	matches := computeMatches(input, start, word, fs, cwd)
	if len(matches) == 0 {
		return input, cursor
	}

	c.active = true
	c.matches = matches
	c.cycleIndex = 0
	c.originalWord = word
	c.wordStart = start
	c.insertedLen = len([]rune(word)) // buffer currently has the original word

	return c.apply(input)
}

// ShiftTab cycles backward through matches.
func (c *Completer) ShiftTab(input []rune, cursor int) ([]rune, int) {
	if !c.active || len(c.matches) == 0 {
		return input, cursor
	}
	c.cycleIndex = (c.cycleIndex - 1 + len(c.matches)) % len(c.matches)
	return c.apply(input)
}

// apply replaces the current word in input with matches[cycleIndex].
func (c *Completer) apply(input []rune) ([]rune, int) {
	match := []rune(c.matches[c.cycleIndex])

	newInput := make([]rune, 0, len(input)-c.insertedLen+len(match))
	newInput = append(newInput, input[:c.wordStart]...)
	newInput = append(newInput, match...)
	newInput = append(newInput, input[c.wordStart+c.insertedLen:]...)

	c.insertedLen = len(match)
	newCursor := c.wordStart + len(match)
	return newInput, newCursor
}

// extractWord returns the word at the cursor position and its start index (in runes).
func extractWord(input []rune, cursor int) (string, int) {
	if cursor > len(input) {
		cursor = len(input)
	}
	start := cursor
	for start > 0 {
		ch := input[start-1]
		if ch == ' ' || ch == '|' || ch == '\t' {
			break
		}
		start--
	}
	return string(input[start:cursor]), start
}

// computeMatches determines completion candidates based on context.
func computeMatches(input []rune, wordStart int, word string, fs FS, cwd string) []string {
	before := strings.TrimSpace(string(input[:wordStart]))

	if isCommandPosition(before) {
		return matchCommands(word, fs)
	}

	return matchPaths(word, fs, cwd)
}

// isCommandPosition returns true if the cursor expects a command name.
func isCommandPosition(before string) bool {
	if before == "" {
		return true
	}
	trimmed := before
	if idx := strings.LastIndex(trimmed, "|"); idx >= 0 {
		afterPipe := strings.TrimSpace(trimmed[idx+1:])
		return afterPipe == ""
	}
	return false
}

// matchCommands returns commands matching the prefix.
func matchCommands(prefix string, fs FS) []string {
	var matches []string
	lower := strings.ToLower(prefix)

	for _, cmd := range builtinCommands {
		if strings.HasPrefix(cmd, lower) {
			matches = append(matches, cmd)
		}
	}

	// /bin/ functions from FS.
	if fs != nil {
		if names, err := fs.ReadDir("/bin"); err == nil {
			for _, name := range names {
				if strings.HasPrefix(strings.ToLower(name), lower) {
					matches = append(matches, name)
				}
			}
		}
	}

	sort.Strings(matches)
	return dedup(matches)
}

// matchPaths returns filesystem path completions for the word.
func matchPaths(word string, fs FS, cwd string) []string {
	if fs == nil {
		return nil
	}

	var dir, namePrefix string
	if strings.Contains(word, "/") {
		if strings.HasSuffix(word, "/") {
			dir = strings.TrimSuffix(word, "/")
			namePrefix = ""
		} else {
			dir = path.Dir(word)
			namePrefix = path.Base(word)
		}
	} else {
		dir = ""
		namePrefix = word
	}

	// Resolve directory against cwd.
	var absDir string
	if dir == "" {
		absDir = cwd
	} else if strings.HasPrefix(dir, "/") {
		absDir = dir
	} else if cwd == "/" {
		absDir = "/" + dir
	} else {
		absDir = cwd + "/" + dir
	}

	entries, err := fs.ReadDir(absDir)
	if err != nil {
		return nil
	}

	var matches []string
	lower := strings.ToLower(namePrefix)
	for _, entry := range entries {
		if namePrefix == "" || strings.HasPrefix(strings.ToLower(entry), lower) {
			if dir == "" {
				matches = append(matches, entry)
			} else {
				matches = append(matches, dir+"/"+entry)
			}
		}
	}

	sort.Strings(matches)
	return matches
}

func dedup(sorted []string) []string {
	if len(sorted) == 0 {
		return sorted
	}
	j := 0
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			sorted[j] = s
			j++
		}
	}
	return sorted[:j]
}
