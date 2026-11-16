package setup

import (
	"fmt"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	cursorStyle = lipgloss.NewStyle().Reverse(true)
	selStyle    = lipgloss.NewStyle().Background(lipgloss.Color("5")).Foreground(lipgloss.Color("15")).Bold(true)
	hitStyle    = lipgloss.NewStyle().Background(lipgloss.Color("2")).Foreground(lipgloss.Color("0"))
	flagStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// pickerModel lets the user move a cursor over one log line with vim motions
// and select the value they are after.
//
//	h l ← →   move one character        0 ^ $   line start / first word / end
//	w b e     word motions              W B E   space-delimited WORD motions
//	v         start/stop selecting      enter   accept the selection
//	          (with no selection, enter takes the ID-like token under the cursor)
//	j n ↓     not on this line: next    k p ↑   previous line
//	s q esc   skip this field
type pickerModel struct {
	title  string
	hint   string
	lineNo int // 0-based, for display
	total  int
	runes  []rune
	cur    int
	anchor int // -1 when not selecting
	width  int
	notice string

	done   bool
	result PickResult
}

func newPicker(title, hint, line string, lineNo, total int) *pickerModel {
	return &pickerModel{title: title, hint: hint, lineNo: lineNo, total: total, runes: []rune(line), anchor: -1, width: 80}
}

func (m *pickerModel) Init() tea.Cmd { return nil }

func (m *pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		m.notice = ""
		// Keys typed faster than the terminal is read arrive as one
		// multi-rune message; treat each rune as its own keypress.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			for _, r := range msg.Runes {
				if m.done {
					break
				}
				m.key(string(r))
			}
		} else {
			m.key(msg.String())
		}
	}
	if m.done {
		return m, tea.Quit
	}
	return m, nil
}

func (m *pickerModel) finish(r PickResult) {
	m.done = true
	m.result = r
}

func (m *pickerModel) key(k string) {
	last := len(m.runes) - 1
	switch k {
	case "h", "left":
		m.cur = max(m.cur-1, 0)
	case "l", "right":
		m.cur = min(m.cur+1, max(last, 0))
	case "0", "home":
		m.cur = 0
	case "^":
		m.cur = m.firstNonSpace()
	case "$", "end":
		m.cur = max(last, 0)
	case "w":
		m.cur = m.wordForward(false)
	case "W":
		m.cur = m.wordForward(true)
	case "b":
		m.cur = m.wordBack(false)
	case "B":
		m.cur = m.wordBack(true)
	case "e":
		m.cur = m.wordEnd(false)
	case "E":
		m.cur = m.wordEnd(true)
	case "v":
		if m.anchor >= 0 {
			m.anchor = -1
		} else {
			m.anchor = m.cur
		}
	case "enter", "y":
		m.accept()
	case "j", "n", "down":
		m.finish(PickResult{Kind: PickNextLine, Line: m.lineNo})
	case "k", "p", "up":
		m.finish(PickResult{Kind: PickPrevLine, Line: m.lineNo})
	case "esc":
		if m.anchor >= 0 {
			m.anchor = -1
		} else {
			m.finish(PickResult{Kind: PickSkip, Line: m.lineNo})
		}
	case "s", "q", "ctrl+c":
		m.finish(PickResult{Kind: PickSkip, Line: m.lineNo})
	}
}

// accept selects the visual selection, or the ID-like token under the cursor.
func (m *pickerModel) accept() {
	lo, hi := m.cur, m.cur
	if m.anchor >= 0 {
		lo, hi = min(m.anchor, m.cur), max(m.anchor, m.cur)
	} else {
		if len(m.runes) == 0 || !isIDRune(m.runes[m.cur]) {
			m.notice = "nothing to select here; move onto the value, or press v and extend"
			return
		}
		for lo > 0 && isIDRune(m.runes[lo-1]) {
			lo--
		}
		for hi < len(m.runes)-1 && isIDRune(m.runes[hi+1]) {
			hi++
		}
	}
	m.finish(PickResult{
		Kind: PickSelected, Line: m.lineNo,
		Start: len(string(m.runes[:lo])), End: len(string(m.runes[:hi+1])),
	})
}

// isIDRune is what an identifier-like value (hex id, uuid, request id) is made of.
func isIDRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.'
}

// class: 0 space, 1 word character, 2 punctuation. With bigWord, anything
// that is not space is one class.
func (m *pickerModel) class(i int, bigWord bool) int {
	r := m.runes[i]
	switch {
	case unicode.IsSpace(r):
		return 0
	case bigWord:
		return 1
	case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
		return 1
	}
	return 2
}

func (m *pickerModel) firstNonSpace() int {
	for i, r := range m.runes {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return 0
}

func (m *pickerModel) wordForward(big bool) int {
	n := len(m.runes)
	if n == 0 {
		return 0
	}
	i := m.cur
	if c := m.class(i, big); c != 0 {
		for i < n && m.class(i, big) == c {
			i++
		}
	}
	for i < n && m.class(i, big) == 0 {
		i++
	}
	return min(i, n-1)
}

func (m *pickerModel) wordBack(big bool) int {
	if len(m.runes) == 0 {
		return 0
	}
	i := m.cur
	if i > 0 {
		i--
	}
	for i > 0 && m.class(i, big) == 0 {
		i--
	}
	c := m.class(i, big)
	for i > 0 && m.class(i-1, big) == c {
		i--
	}
	return i
}

func (m *pickerModel) wordEnd(big bool) int {
	n := len(m.runes)
	if n == 0 {
		return 0
	}
	i := m.cur + 1
	for i < n && m.class(i, big) == 0 {
		i++
	}
	if i >= n {
		return n - 1
	}
	c := m.class(i, big)
	for i+1 < n && m.class(i+1, big) == c {
		i++
	}
	return i
}

func (m *pickerModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", labelStyle.Render(m.title), hintStyle.Render(fmt.Sprintf("line %d/%d", m.lineNo+1, m.total)))
	fmt.Fprintf(&b, "%s\n\n", m.hint)
	b.WriteString(m.renderLine())
	b.WriteString("\n\n")
	if m.notice != "" {
		b.WriteString(flagStyle.Render(m.notice) + "\n")
	}
	b.WriteString(hintStyle.Render("h l w b e 0 $  move   v  select   enter  accept   j  not on this line   k  previous   s  skip field"))
	b.WriteString("\n")
	return b.String()
}

// renderLine draws the line, windowed horizontally around the cursor, with
// the selection and cursor styled.
func (m *pickerModel) renderLine() string {
	avail := max(m.width-2, 10)
	start := 0
	if len(m.runes) > avail {
		start = min(max(m.cur-avail/2, 0), len(m.runes)-avail)
	}
	end := min(start+avail, len(m.runes))
	lo, hi := -1, -1
	if m.anchor >= 0 {
		lo, hi = min(m.anchor, m.cur), max(m.anchor, m.cur)
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(hintStyle.Render("…"))
	}
	for i := start; i < end; i++ {
		s := string(m.runes[i])
		switch {
		case i == m.cur:
			s = cursorStyle.Render(s)
		case i >= lo && i <= hi:
			s = selStyle.Render(s)
		}
		b.WriteString(s)
	}
	if len(m.runes) == 0 {
		b.WriteString(cursorStyle.Render(" "))
	}
	if end < len(m.runes) {
		b.WriteString(hintStyle.Render("…"))
	}
	return b.String()
}
