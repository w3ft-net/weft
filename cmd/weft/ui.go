package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// noColor is set by the global --nocolor flag. Color is also off whenever the
// destination is not a terminal or NO_COLOR is set (lipgloss/termenv decide
// that per writer), so piped output is always plain.
var noColor bool

// styles is the CLI's color vocabulary, drawn from the weft logo palette (the
// same 256-color picks as theme.Palette): azure chrome, light-blue names,
// gold for a filling meter, orange for work in flight, bold green for
// "ready / done", salmon for errors. Every non-shell command renders through
// it so the tables, status words and messages agree.
type styles struct {
	head, name, muted, count, filled, empty, active, ready, ok, err, warn, preview lipgloss.Style
}

// newStyles builds styles for output headed to w, honoring --nocolor.
func newStyles(w io.Writer) styles {
	r := lipgloss.NewRenderer(w)
	if noColor {
		r.SetColorProfile(termenv.Ascii)
	}
	return stylesFor(r)
}

func stylesFor(r *lipgloss.Renderer) styles {
	fg := func(c string) lipgloss.Style { return r.NewStyle().Foreground(lipgloss.Color(c)) }
	return styles{
		head:    fg("39").Bold(true),
		name:    fg("75").Bold(true),
		muted:   fg("110"),
		count:   fg("105"),
		filled:  fg("214"),
		empty:   fg("103"),
		active:  fg("208"),
		ready:   fg("2").Bold(true),
		ok:      fg("2"),
		err:     fg("209").Bold(true),
		warn:    fg("208"),
		preview: fg("110").Italic(true),
	}
}

func outStyles() styles { return newStyles(os.Stdout) }
func errStyles() styles { return newStyles(os.Stderr) }

// paint styles s without disturbing leading or trailing newlines (a styled
// "\ndone\n" must stay "\ndone\n" so message spacing survives).
func paint(st lipgloss.Style, s string) string {
	body := strings.Trim(s, "\n")
	if body == "" {
		return s
	}
	lead := s[:strings.Index(s, body)]
	return lead + st.Render(body) + s[len(lead)+len(body):]
}

func errorf(format string, a ...any) {
	fmt.Fprint(os.Stderr, paint(errStyles().err, fmt.Sprintf(format, a...)))
}
func errorln(a ...any) { fmt.Fprint(os.Stderr, paint(errStyles().err, fmt.Sprintln(a...))) }
func warnf(format string, a ...any) {
	fmt.Fprint(os.Stderr, paint(errStyles().warn, fmt.Sprintf(format, a...)))
}
func usageln(a ...any) { fmt.Fprint(os.Stderr, paint(errStyles().muted, fmt.Sprintln(a...))) }

func successf(format string, a ...any) {
	fmt.Print(paint(outStyles().ok, fmt.Sprintf(format, a...)))
}
func hintf(format string, a ...any) {
	fmt.Print(paint(outStyles().muted, fmt.Sprintf(format, a...)))
}
func hintln(a ...any) { fmt.Print(paint(outStyles().muted, fmt.Sprintln(a...))) }

// cell pads plain text to width, then styles it, so ANSI codes never skew
// column alignment.
func cell(st lipgloss.Style, text string, width int) string {
	if pad := width - len([]rune(text)); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return st.Render(text)
}

// statusStyle colors a status word: calm for steady states, orange while
// work is in flight, bold green when a human can act, salmon when broken.
func (s styles) statusStyle(status string) lipgloss.Style {
	switch {
	case status == "pending" || status == "resampled":
		return s.ready
	case strings.HasPrefix(status, "sampling"), strings.HasPrefix(status, "resampling"):
		return s.active
	case status == "revoked", status == "expired", status == "error", status == "failed":
		return s.err
	case status == "active", status == "valid":
		return s.ok
	}
	return s.muted
}

// column is one table column. A zero width means "last column, unpadded".
type column struct {
	title string
	width int
	style func(s styles, v string) lipgloss.Style
}

func nameCol(title string, width int) column {
	return column{title, width, func(s styles, _ string) lipgloss.Style { return s.name }}
}
func mutedCol(title string, width int) column {
	return column{title, width, func(s styles, _ string) lipgloss.Style { return s.muted }}
}
func statusCol(title string, width int) column {
	return column{title, width, func(s styles, v string) lipgloss.Style { return s.statusStyle(v) }}
}

// writeTable prints a header row and rows, indented, colored per column.
func writeTable(w io.Writer, st styles, indent string, cols []column, rows [][]string) {
	line := func(vals []string, style func(c column, v string) lipgloss.Style) {
		parts := make([]string, len(cols))
		for i, c := range cols {
			v := ""
			if i < len(vals) {
				v = vals[i]
			}
			if c.width == 0 {
				parts[i] = style(c, v).Render(v)
			} else {
				parts[i] = cell(style(c, v), v, c.width)
			}
		}
		fmt.Fprintln(w, indent+strings.TrimRight(strings.Join(parts, " "), " "))
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	line(titles, func(column, string) lipgloss.Style { return st.head })
	for _, r := range rows {
		line(r, func(c column, v string) lipgloss.Style {
			if c.style == nil {
				return st.muted
			}
			return c.style(st, v)
		})
	}
}

// colorDiff tints a unified diff: additions green, removals salmon, hunk and
// file headers azure.
func (s styles) colorDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			lines[i] = s.head.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = s.ok.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = s.err.UnsetBold().Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// kv prints an aligned "Label: value" pair with the label muted.
func (s styles) kv(label string, width int, value string) string {
	return cell(s.muted, label+":", width) + " " + value
}
