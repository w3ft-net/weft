package shellcore

import (
	"fmt"
	"strings"
)

// lessState holds the active pager session. Content is held as
// already-wrapped display lines — lines longer than the viewport width
// are pre-wrapped at enter time so scrolling is O(1) in line count and
// we never have to re-measure glyph widths during navigation.
type lessState struct {
	lines  []string // wrapped display lines
	offset int      // top line index (0-based)
	width  int      // viewport width at enter time
	height int      // viewport height at enter time
	title  string   // shown on the status line ("man alert", "help cron", or "")

	// Search state.
	searching bool     // actively typing a query (at the : prompt)
	query     string   // the committed search query
	queryBuf  string   // the query currently being typed
	matches   []int    // line indices that contain query; ascending
	matchIdx  int      // position within matches[]

	// Transient status message ("Pattern not found", etc.). Cleared on
	// the next non-status keystroke.
	status string
}

// enterLess transitions the shell into ModeLess with the supplied
// content. title is shown on the status line (empty string = no
// title). Caller is responsible for ensuring there *is* content
// worth paging — this doesn't second-guess short input.
func (c *ShellCore) enterLess(text, title string) {
	lines := wrapLines(text, c.width)
	c.less = &lessState{
		lines:  lines,
		width:  c.width,
		height: c.height,
		title:  title,
	}
	c.mode = ModeLess
	c.renderLess()
}

// wrapLines splits text on \n then wraps each logical line at width.
// Trailing blank lines are preserved (useful for code blocks in man
// pages). Tabs expand to 8 spaces — the simplest thing that doesn't
// surprise anyone.
func wrapLines(text string, width int) []string {
	if width < 1 {
		width = 80
	}
	text = strings.ReplaceAll(text, "\t", "        ")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	raw := strings.Split(text, "\n")
	// Drop exactly one trailing blank line produced by a terminal
	// newline — the content itself would never be read as "one empty
	// page at the end".
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1]
	}
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" {
			out = append(out, "")
			continue
		}
		for visualLen(line) > width {
			// Break at width (no word wrap — matches how real less
			// truncates when "-S" is off; we default to chop-long
			// because our content is often ANSI-decorated and naive
			// word wrap would split escape sequences).
			cut := chopAt(line, width)
			out = append(out, line[:cut])
			line = line[cut:]
		}
		out = append(out, line)
	}
	return out
}

// visualLen returns the display width of s, ignoring CSI sequences
// (ESC [ ... letter). Counts runes, not bytes, so multibyte content
// isn't double-counted.
func visualLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			// CSI ends at an alphabetic byte in the 0x40-0x7E range.
			if (r >= 0x40 && r <= 0x7E) && r != '[' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// chopAt returns the byte offset at which to cut s so the visible
// prefix fits in width columns. Preserves ANSI escape sequences.
func chopAt(s string, width int) int {
	n := 0
	inEsc := false
	for i, r := range s {
		if inEsc {
			if (r >= 0x40 && r <= 0x7E) && r != '[' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		n++
		if n > width {
			return i
		}
	}
	return len(s)
}

// handleLessKey processes a keystroke in ModeLess.
func (c *ShellCore) handleLessKey(ev KeyEvent) {
	if c.less == nil {
		return
	}
	l := c.less

	if l.searching {
		c.handleLessSearchKey(ev)
		return
	}

	// Any non-status keystroke clears a transient status message.
	l.status = ""

	pageStep := l.height - 2 // save a line for status, one for overlap
	if pageStep < 1 {
		pageStep = 1
	}

	switch ev.Key {
	case KeyPgDown:
		c.scrollLess(pageStep)
		return
	case KeyPgUp:
		c.scrollLess(-pageStep)
		return
	case KeyDown:
		c.scrollLess(1)
		return
	case KeyUp:
		c.scrollLess(-1)
		return
	case KeyCtrlC:
		c.exitLess()
		return
	}

	if ev.Rune == 0 {
		return
	}

	switch ev.Rune {
	case 'q', 'Q':
		c.exitLess()
	case ' ', 'f':
		c.scrollLess(pageStep)
	case 'b':
		c.scrollLess(-pageStep)
	case 'j':
		c.scrollLess(1)
	case 'k':
		c.scrollLess(-1)
	case 'g':
		// Two presses of g (gg) jump to top; a single g also does in
		// our shell (we don't support numeric counts, so there's no
		// ambiguity with a pending count prefix).
		l.offset = 0
		c.renderLess()
	case 'G':
		c.scrollToBottom()
	case '/':
		l.searching = true
		l.queryBuf = ""
		c.renderLess()
	case 'n':
		c.jumpToMatch(1)
	case 'N':
		c.jumpToMatch(-1)
	}
}

// handleLessSearchKey accumulates a search query and commits on Enter.
func (c *ShellCore) handleLessSearchKey(ev KeyEvent) {
	l := c.less
	switch ev.Key {
	case KeyEnter:
		l.searching = false
		l.query = l.queryBuf
		l.queryBuf = ""
		c.computeMatches()
		if len(l.matches) == 0 {
			l.status = fmt.Sprintf("Pattern not found: %s", l.query)
			c.renderLess()
			return
		}
		// Jump to the first match at or below current offset.
		l.matchIdx = 0
		for i, idx := range l.matches {
			if idx >= l.offset {
				l.matchIdx = i
				break
			}
		}
		l.offset = l.matches[l.matchIdx]
		c.clampOffset()
		c.renderLess()
		return
	case KeyEsc, KeyCtrlC:
		l.searching = false
		l.queryBuf = ""
		c.renderLess()
		return
	case KeyBackspace:
		if len(l.queryBuf) > 0 {
			l.queryBuf = l.queryBuf[:len(l.queryBuf)-1]
		}
		c.renderLess()
		return
	}
	if ev.Rune != 0 && ev.Rune >= 0x20 {
		l.queryBuf += string(ev.Rune)
		c.renderLess()
	}
}

// computeMatches rebuilds the match list for the current query.
func (c *ShellCore) computeMatches() {
	l := c.less
	l.matches = l.matches[:0]
	if l.query == "" {
		return
	}
	for i, line := range l.lines {
		if strings.Contains(line, l.query) {
			l.matches = append(l.matches, i)
		}
	}
}

// jumpToMatch moves forward (dir=1) or backward (dir=-1) through
// matches, wrapping around. No-op if there are no matches.
func (c *ShellCore) jumpToMatch(dir int) {
	l := c.less
	if len(l.matches) == 0 {
		if l.query != "" {
			l.status = fmt.Sprintf("Pattern not found: %s", l.query)
			c.renderLess()
		}
		return
	}
	l.matchIdx = (l.matchIdx + dir + len(l.matches)) % len(l.matches)
	l.offset = l.matches[l.matchIdx]
	c.clampOffset()
	c.renderLess()
}

func (c *ShellCore) scrollLess(delta int) {
	c.less.offset += delta
	c.clampOffset()
	c.renderLess()
}

func (c *ShellCore) scrollToBottom() {
	l := c.less
	l.offset = len(l.lines) - (l.height - 1)
	c.clampOffset()
	c.renderLess()
}

// clampOffset keeps offset in [0, max] where max is the last position
// at which a full viewport's worth of content is still visible, or 0
// if the whole buffer fits in one screen.
func (c *ShellCore) clampOffset() {
	l := c.less
	max := len(l.lines) - (l.height - 1)
	if max < 0 {
		max = 0
	}
	if l.offset < 0 {
		l.offset = 0
	}
	if l.offset > max {
		l.offset = max
	}
}

// exitLess tears down the pager and redraws the shell prompt.
func (c *ShellCore) exitLess() {
	c.less = nil
	c.mode = ModeInput
	c.write([]byte("\x1b[2J\x1b[H")) // clear screen
	c.resetFlushState()
	c.Redraw()
}

// renderLess paints the current viewport. The bottom row is a status
// bar showing title + percent, search prompt, or a transient status.
func (c *ShellCore) renderLess() {
	l := c.less
	if l == nil {
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J") // home + clear

	contentHeight := l.height - 1
	if contentHeight < 1 {
		contentHeight = 1
	}

	end := l.offset + contentHeight
	if end > len(l.lines) {
		end = len(l.lines)
	}
	for i := l.offset; i < end; i++ {
		line := l.lines[i]
		if l.query != "" && strings.Contains(line, l.query) {
			line = highlightMatches(line, l.query)
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	// Fill unused rows with tildes so the viewport height is stable.
	for i := end - l.offset; i < contentHeight; i++ {
		b.WriteString(ansiDim)
		b.WriteString("~")
		b.WriteString(ansiReset)
		b.WriteString("\r\n")
	}

	// Status bar.
	b.WriteString("\x1b[7m") // reverse video
	switch {
	case l.searching:
		b.WriteByte('/')
		b.WriteString(l.queryBuf)
		b.WriteString(strings.Repeat(" ", max0(l.width-1-len(l.queryBuf))))
	case l.status != "":
		msg := l.status
		if len(msg) > l.width {
			msg = msg[:l.width]
		}
		b.WriteString(msg)
		b.WriteString(strings.Repeat(" ", max0(l.width-len(msg))))
	default:
		left := l.title
		if left == "" {
			left = "(weft less)"
		}
		right := formatPercent(l.offset, len(l.lines), contentHeight)
		pad := l.width - len(left) - len(right)
		if pad < 0 {
			pad = 0
			// Truncate left if very narrow.
			if len(left) > l.width-len(right) {
				left = left[:max0(l.width-len(right))]
			}
		}
		b.WriteString(left)
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString(right)
	}
	b.WriteString(ansiReset)
	c.write([]byte(b.String()))
}

// highlightMatches wraps every occurrence of query in s with ANSI
// reverse-video. Applied to already-rendered lines, so escape
// sequences in the surrounding line are left intact.
func highlightMatches(s, query string) string {
	if query == "" {
		return s
	}
	var b strings.Builder
	rest := s
	for {
		idx := strings.Index(rest, query)
		if idx < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:idx])
		b.WriteString("\x1b[7m")
		b.WriteString(query)
		b.WriteString("\x1b[27m") // reset inverse only
		rest = rest[idx+len(query):]
	}
	return b.String()
}

// formatPercent returns "N%" where N is the position of the bottom
// visible line within the buffer. Shows "END" once the last line
// is on screen, "Top" when still at the top of a multi-page buffer,
// and "All" when the buffer fits in one screen.
func formatPercent(offset, total, contentHeight int) string {
	if total <= contentHeight {
		return "(All)"
	}
	if offset == 0 {
		return "(Top)"
	}
	bottom := offset + contentHeight
	if bottom >= total {
		return "(END)"
	}
	pct := (bottom * 100) / total
	return fmt.Sprintf("(%d%%)", pct)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
