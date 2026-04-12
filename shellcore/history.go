package shellcore

import "strings"

// History tracks command history with cursor navigation.
type History struct {
	entries []string
	cursor  int    // -1 = not navigating; 0..len-1 = browsing history
	saved   string // the in-progress input before the user started navigating
}

// Add appends a line to history, deduplicating consecutive entries.
func (h *History) Add(line string) {
	if line == "" {
		return
	}
	if len(h.entries) > 0 && h.entries[len(h.entries)-1] == line {
		return
	}
	h.entries = append(h.entries, line)
	h.cursor = -1
}

// Up moves to the previous history entry. Returns the entry and true if moved.
func (h *History) Up(currentInput string) (string, bool) {
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

// Down moves to the next history entry. Returns the entry and true if moved.
func (h *History) Down(currentInput string) (string, bool) {
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

// Reset clears the navigation cursor.
func (h *History) Reset() {
	h.cursor = -1
}

// Search finds the most recent entry containing query, searching backwards
// from fromIdx. Returns the match and its index, or ("", -1) if not found.
func (h *History) Search(query string, fromIdx int) (string, int) {
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

// Len returns the number of history entries.
func (h *History) Len() int {
	return len(h.entries)
}
