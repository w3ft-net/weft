package theme

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TraceListRow is one line of a bare `trace`, parsed. Dur and Age are the
// display text, so the terminal and the browser show the same thing.
type TraceListRow struct {
	TraceID  string
	StartNs  int64
	EndNs    int64
	DurNs    int64
	Dur      string
	Age      string // since the trace's last line, relative to the `now` given to ParseTraceList
	Lines    int64
	Errors   int64
	NoLevel  bool // no line carried a level, so Errors is unknown, not zero
	Services []string
	Root     string
}

type traceListJSON struct {
	TraceID  string   `json:"trace_id"`
	StartNs  int64    `json:"start_ns"`
	EndNs    int64    `json:"end_ns"`
	DurNs    int64    `json:"dur_ns"`
	Lines    int64    `json:"lines"`
	Errors   *int64   `json:"errors"`
	Services []string `json:"services"`
	Root     string   `json:"root"`
}

// ParseTraceList reads the JSON lines a bare `trace` prints. A line that
// isn't one is skipped.
func ParseTraceList(now time.Time, lines []string) []TraceListRow {
	var rows []TraceListRow
	for _, l := range lines {
		var j traceListJSON
		if json.Unmarshal([]byte(l), &j) != nil || j.TraceID == "" {
			continue
		}
		row := TraceListRow{
			TraceID: j.TraceID, StartNs: j.StartNs, EndNs: j.EndNs, DurNs: j.DurNs,
			Dur: time.Duration(j.DurNs).String(), Age: formatAge(now.Sub(time.Unix(0, j.EndNs))),
			Lines: j.Lines, NoLevel: j.Errors == nil, Services: j.Services, Root: j.Root,
		}
		if j.Errors != nil {
			row.Errors = *j.Errors
		}
		rows = append(rows, row)
	}
	return rows
}

// formatAge is how long ago, in the largest whole unit: 42s, 3m, 5h, 2d.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(max(int(d/time.Second), 0)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

const (
	traceListAgeWidth    = 4
	traceListDurWidth    = 7
	traceListLinesWidth  = 5
	traceListErrorsWidth = 6
	traceListMaxRoot     = 28
	traceListMaxServices = 48
	traceListMinRoom     = 8 // below this ROOT and SERVICES are not worth showing
)

// RenderTraceList lays the rows out as a table sized to termWidth, with the
// trace ID last: it is what you hand to `trace --id`, so it is never shortened,
// and as the last column it leaves ROOT and SERVICES whatever room is left. On
// a terminal too narrow for those, they are dropped and the ID keeps its width.
func RenderTraceList(now time.Time, lines []string, termWidth int) []string {
	rows := ParseTraceList(now, lines)
	if len(rows) == 0 {
		return []string{Palette().Comment + "(no traces in this range)" + "\x1b[0m"}
	}
	idW, rootMax, svcMax := len("TRACE"), len("ROOT"), len("SERVICES")
	for i := range rows {
		idW = max(idW, utf8.RuneCountInString(rows[i].TraceID))
		rootMax = max(rootMax, utf8.RuneCountInString(rows[i].Root))
		svcMax = max(svcMax, utf8.RuneCountInString(strings.Join(rows[i].Services, ", ")))
	}
	rootMax, svcMax = min(rootMax, traceListMaxRoot), min(svcMax, traceListMaxServices)

	const sp = 1
	left := traceListAgeWidth + sp + traceListDurWidth + sp + traceListLinesWidth + sp + traceListErrorsWidth
	room := termWidth - left - idW - 2*sp
	rootW, svcW := 0, 0
	switch {
	case room >= rootMax+sp+svcMax:
		rootW, svcW = rootMax, svcMax
	case room >= 3*traceListMinRoom:
		rootW = room * 2 / 5
		svcW = room - sp - rootW
	case room >= traceListMinRoom:
		rootW = room
	}

	cells := func(age, dur, nlines, errs, root, svcs, id string) string {
		s := PadLeftVisible(age, traceListAgeWidth) + " " + PadLeftVisible(dur, traceListDurWidth) +
			" " + PadLeftVisible(nlines, traceListLinesWidth) + " " + PadLeftVisible(errs, traceListErrorsWidth)
		if rootW > 0 {
			s += " " + PadVisible(truncRunes(root, rootW), rootW)
		}
		if svcW > 0 {
			s += " " + PadVisible(truncRunes(svcs, svcW), svcW)
		}
		return s + " " + id
	}

	out := []string{Palette().Key + cells("AGE", "DUR", "LINES", "ERRORS", "ROOT", "SERVICES", "TRACE") + "\x1b[0m"}
	color := func(s, sgr string) string { return sgr + s + FgReset }
	for _, r := range rows {
		errs := PadLeftVisible("-", traceListErrorsWidth)
		switch {
		case r.NoLevel:
			errs = color(errs, Palette().Comment)
		case r.Errors > 0:
			errs = color(PadLeftVisible(strconv.FormatInt(r.Errors, 10), traceListErrorsWidth), Palette().Error)
		default:
			errs = PadLeftVisible("0", traceListErrorsWidth)
		}
		line := color(PadLeftVisible(r.Age, traceListAgeWidth), Palette().Number) + " " +
			color(PadLeftVisible(r.Dur, traceListDurWidth), Palette().Number) + " " +
			color(PadLeftVisible(strconv.FormatInt(r.Lines, 10), traceListLinesWidth), Palette().Number) + " " + errs
		if rootW > 0 {
			line += " " + PadVisible(truncRunes(r.Root, rootW), rootW)
		}
		if svcW > 0 {
			line += " " + color(PadVisible(truncRunes(strings.Join(r.Services, ", "), svcW), svcW), Palette().Comment)
		}
		out = append(out, line+" "+color(r.TraceID, Palette().Key))
	}
	return out
}

// truncRunes cuts s to w runes, ending in an ellipsis when it had to.
func truncRunes(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w <= 1 {
		return string([]rune(s)[:max(w, 0)])
	}
	return string([]rune(s)[:w-1]) + "…"
}
