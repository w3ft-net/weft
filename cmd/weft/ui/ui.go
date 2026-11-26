// Package ui is the CLI's output vocabulary: colors drawn from the weft logo
// palette, status words, tables and the one-line messages. Every non-shell
// command renders through it so they agree.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// NoColor is set by the global --nocolor flag. Color is also off whenever the
// destination is not a terminal or NO_COLOR is set (lipgloss/termenv decide
// that per writer), so piped output is always plain.
var NoColor bool

// Styles is the CLI's color vocabulary, drawn from the weft logo palette (the
// same 256-color picks as theme.Palette): azure chrome, light-blue names,
// gold for a filling meter, orange for work in flight, bold green for
// "ready / done", salmon for errors. Every non-shell command renders through
// it so the tables, status words and messages agree.
type Styles struct {
	Head, Name, Muted, Count, Filled, Empty, Active, Ready, OK, Err, Warn, Preview lipgloss.Style
}

// NewStyles builds Styles for output headed to w, honoring --nocolor.
func NewStyles(w io.Writer) Styles {
	r := lipgloss.NewRenderer(w)
	if NoColor {
		r.SetColorProfile(termenv.Ascii)
	}
	return StylesFor(r)
}

func StylesFor(r *lipgloss.Renderer) Styles {
	fg := func(c string) lipgloss.Style { return r.NewStyle().Foreground(lipgloss.Color(c)) }
	return Styles{
		Head:    fg("39").Bold(true),
		Name:    fg("75").Bold(true),
		Muted:   fg("110"),
		Count:   fg("105"),
		Filled:  fg("214"),
		Empty:   fg("103"),
		Active:  fg("208"),
		Ready:   fg("2").Bold(true),
		OK:      fg("2"),
		Err:     fg("209").Bold(true),
		Warn:    fg("208"),
		Preview: fg("110").Italic(true),
	}
}

func Out() Styles       { return NewStyles(os.Stdout) }
func ErrStyles() Styles { return NewStyles(os.Stderr) }

// Paint styles s without disturbing leading or trailing newlines (a styled
// "\ndone\n" must stay "\ndone\n" so message spacing survives).
func Paint(st lipgloss.Style, s string) string {
	body := strings.Trim(s, "\n")
	if body == "" {
		return s
	}
	lead := s[:strings.Index(s, body)]
	return lead + st.Render(body) + s[len(lead)+len(body):]
}

func Errorf(format string, a ...any) {
	fmt.Fprint(os.Stderr, Paint(ErrStyles().Err, fmt.Sprintf(format, a...)))
}
func Errorln(a ...any) { fmt.Fprint(os.Stderr, Paint(ErrStyles().Err, fmt.Sprintln(a...))) }
func Warnf(format string, a ...any) {
	fmt.Fprint(os.Stderr, Paint(ErrStyles().Warn, fmt.Sprintf(format, a...)))
}
func Usageln(a ...any) { fmt.Fprint(os.Stderr, Paint(ErrStyles().Muted, fmt.Sprintln(a...))) }

func Successf(format string, a ...any) {
	fmt.Print(Paint(Out().OK, fmt.Sprintf(format, a...)))
}
func Hintf(format string, a ...any) {
	fmt.Print(Paint(Out().Muted, fmt.Sprintf(format, a...)))
}
func Hintln(a ...any) { fmt.Print(Paint(Out().Muted, fmt.Sprintln(a...))) }

// Cell pads plain text to width, then styles it, so ANSI codes never skew
// column alignment.
func Cell(st lipgloss.Style, text string, width int) string {
	if pad := width - len([]rune(text)); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return st.Render(text)
}

// StatusStyle colors a status word: calm for steady states, orange while
// work is in flight, bold green when a human can act, salmon when broken.
func (s Styles) StatusStyle(status string) lipgloss.Style {
	switch {
	case status == "pending" || status == "resampled":
		return s.Ready
	case strings.HasPrefix(status, "sampling"), strings.HasPrefix(status, "resampling"):
		return s.Active
	case status == "revoked", status == "expired", status == "error", status == "failed":
		return s.Err
	case status == "active", status == "valid":
		return s.OK
	}
	return s.Muted
}

// Column is one table column. A zero width means "last column, unpadded".
type Column struct {
	Title string
	Width int
	Style func(s Styles, v string) lipgloss.Style
}

func NameCol(title string, width int) Column {
	return Column{title, width, func(s Styles, _ string) lipgloss.Style { return s.Name }}
}
func MutedCol(title string, width int) Column {
	return Column{title, width, func(s Styles, _ string) lipgloss.Style { return s.Muted }}
}
func StatusCol(title string, width int) Column {
	return Column{title, width, func(s Styles, v string) lipgloss.Style { return s.StatusStyle(v) }}
}

// WriteTable prints a header row and rows, indented, colored per column.
func WriteTable(w io.Writer, st Styles, indent string, cols []Column, rows [][]string) {
	line := func(vals []string, style func(c Column, v string) lipgloss.Style) {
		parts := make([]string, len(cols))
		for i, c := range cols {
			v := ""
			if i < len(vals) {
				v = vals[i]
			}
			if c.Width == 0 {
				parts[i] = style(c, v).Render(v)
			} else {
				parts[i] = Cell(style(c, v), v, c.Width)
			}
		}
		fmt.Fprintln(w, indent+strings.TrimRight(strings.Join(parts, " "), " "))
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.Title
	}
	line(titles, func(Column, string) lipgloss.Style { return st.Head })
	for _, r := range rows {
		line(r, func(c Column, v string) lipgloss.Style {
			if c.Style == nil {
				return st.Muted
			}
			return c.Style(st, v)
		})
	}
}

// ColorDiff tints a unified diff: additions green, removals salmon, hunk and
// file headers azure.
func (s Styles) ColorDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			lines[i] = s.Head.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = s.OK.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = s.Err.UnsetBold().Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// KV prints an aligned "Label: value" pair with the label muted.
func (s Styles) KV(label string, width int, value string) string {
	return Cell(s.Muted, label+":", width) + " " + value
}
