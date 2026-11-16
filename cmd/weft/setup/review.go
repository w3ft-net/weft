package setup

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/w3ft-net/weft/ingest"
)

// reviewModel shows an extractor's effect on every sample line and lets the
// user flag lines it got wrong.
//
//	j k ↓ ↑  move   g G  top / bottom
//	m        the rule missed this line (then highlight the value on it)
//	x        the rule got this line wrong (it should have no value)
//	a enter  accept the rule     r  start over     s q esc  skip this field
type reviewModel struct {
	title  string
	lines  []string
	ex     *ingest.ProbeExtractor
	res    *ingest.ProbeResult
	marks  map[int]ingest.ProbeMark
	cur    int
	width  int
	height int

	done   bool
	result ReviewResult
}

func newReview(title string, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult, marks []ingest.ProbeMark) *reviewModel {
	m := &reviewModel{title: title, lines: lines, ex: ex, res: res, marks: map[int]ingest.ProbeMark{}, width: 80, height: 24}
	for _, mk := range marks {
		m.marks[mk.Line] = mk
	}
	// Start on the first line that needs attention: a miss.
	for i, r := range res.Results {
		if !r.Matched {
			m.cur = i
			break
		}
	}
	return m
}

func (m *reviewModel) Init() tea.Cmd { return nil }

func (m *reviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
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

func (m *reviewModel) key(k string) {
	switch k {
	case "j", "down":
		m.cur = min(m.cur+1, len(m.lines)-1)
	case "k", "up":
		m.cur = max(m.cur-1, 0)
	case "g", "home":
		m.cur = 0
	case "G", "end":
		m.cur = len(m.lines) - 1
	case "m":
		m.finish(ReviewResult{Kind: ReviewFlagMissing, Line: m.cur})
	case "x":
		m.finish(ReviewResult{Kind: ReviewFlagWrong, Line: m.cur})
	case "a", "enter":
		m.finish(ReviewResult{Kind: ReviewAccept})
	case "r":
		m.finish(ReviewResult{Kind: ReviewRestart})
	case "s", "q", "esc", "ctrl+c":
		m.finish(ReviewResult{Kind: ReviewSkip})
	}
}

func (m *reviewModel) finish(r ReviewResult) {
	m.done = true
	m.result = r
}

func (m *reviewModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", labelStyle.Render(m.title),
		hintStyle.Render(fmt.Sprintf("matched %d of %d lines", m.res.Matched, m.res.Total)))
	rule := describeExtractor(m.ex)
	if m.res.Source == "model" {
		rule += "  (suggested by the model; the lines below show what it does)"
	}
	fmt.Fprintf(&b, "%s\n\n", hintStyle.Render(rule))

	rows := max(m.height-7, 5)
	top := min(max(m.cur-rows/2, 0), max(len(m.lines)-rows, 0))
	for i := top; i < min(top+rows, len(m.lines)); i++ {
		mark := "  "
		switch mk, ok := m.marks[i]; {
		case ok && mk.None:
			mark = flagStyle.Render("x ")
		case ok:
			mark = hitStyle.Render("✓ ")
		case m.res.Results[i].Matched:
			mark = "✓ "
		default:
			mark = hintStyle.Render("· ")
		}
		pointer := "  "
		if i == m.cur {
			pointer = "> "
		}
		fmt.Fprintf(&b, "%s%s%s\n", pointer, mark, m.row(i))
	}
	b.WriteString("\n" + hintStyle.Render("j k  move   m  missed this line   x  wrong on this line   a  accept   r  start over   s  skip field") + "\n")
	return b.String()
}

// row renders line i, windowed so the extracted value is visible, with the
// value highlighted.
func (m *reviewModel) row(i int) string {
	return renderRow(m.lines[i], m.res.Results[i], max(m.width-6, 20))
}

// renderRow draws line within width runes, scrolled so the extracted value
// (if any) is in view, with the value highlighted.
func renderRow(line string, r ingest.ProbeLineResult, avail int) string {
	runes := []rune(line)
	hlStart, hlEnd := -1, -1
	if r.Matched && r.Text != "" && strings.HasPrefix(line, r.Prefix) {
		hlStart = len([]rune(r.Prefix))
		hlEnd = hlStart + len([]rune(r.Text))
	}
	start := 0
	if len(runes) > avail {
		anchor := 0
		if hlStart >= 0 {
			anchor = hlStart
		}
		start = min(max(anchor-avail/3, 0), len(runes)-avail)
	}
	end := min(start+avail, len(runes))
	var b strings.Builder
	for j := start; j < end; j++ {
		s := string(runes[j])
		if j >= hlStart && j < hlEnd {
			s = hitStyle.Render(s)
		}
		b.WriteString(s)
	}
	return b.String()
}

// describeExtractor puts an extractor in words for the header.
func describeExtractor(ex *ingest.ProbeExtractor) string {
	if ex == nil {
		return ""
	}
	return ex.Describe()
}
