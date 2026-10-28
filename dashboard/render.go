// Package dashboard renders a parsed store.DashboardSpec into ANSI text
// sized to a terminal viewport. Pure rendering — fetching the spec and
// the per-watch results is the caller's job (see Builtin for the shell
// wiring that bundles both).
package dashboard

import (
	"fmt"
	"strings"

	weftchart "github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
)

// WatchSeries is the data a tile needs to render: the cron's stored points
// over the tile's range (before any tile's aggregate or bucket, which
// each series tile applies itself) and its latest run (counter, gauge and
// table tiles).
type WatchSeries struct {
	Series weftchart.CronSeries
	Latest []weftchart.LatestPoint // the latest run, one point per tag in table order
}

// points are the numbers a counter, gauge or table shows: the cron's latest
// run, or with a range or aggregate its aggregate over the range. One per
// tag, highest first. The string is why the chart can't aggregate, if so.
func (s WatchSeries) points(t store.DashboardTile) ([]weftchart.LatestPoint, string) {
	if !t.Windowed() {
		return s.Latest, ""
	}
	return weftchart.CronTotals(s.Series, t.ChartArgs().Aggregate)
}

// headline is the number a counter or gauge shows: the first numeric point,
// the highest on a tagged cron.
func headline(points []weftchart.LatestPoint) (float64, bool) {
	for _, p := range points {
		if p.Value != nil {
			return *p.Value, true
		}
	}
	return 0, false
}

// Render lays out the spec into a single string sized to (w, h).
// Width is the available terminal width in columns; the renderer
// flows tiles row-major, packing as many as fit per row given the
// size hints. results maps tile.Watch → WatchSeries; missing keys
// render as "<no data>" placeholders.
//
// Below singleColumnThreshold, every tile is laid out one per row
// at full width — at 80×24 a 3-tile row is not legible.
func Render(spec *store.DashboardSpec, results map[string]WatchSeries, w int) string {
	if spec == nil {
		return ""
	}
	if w < 20 {
		w = 20
	}
	var b strings.Builder
	if spec.Title != "" {
		b.WriteString(boldTitle(spec.Title))
		b.WriteString("\n\n")
	}
	for si, sec := range spec.Sections {
		if si > 0 {
			b.WriteByte('\n')
		}
		if sec.Title != "" {
			b.WriteString(sectionHeading(sec.Title, w))
			b.WriteByte('\n')
		}
		renderSection(&b, sec.Tiles, results, w)
	}
	return b.String()
}

const (
	singleColumnThreshold = 60
	tileGap               = 2

	clrOrange = 208
	clrPeach  = 209 // focused tile border
	clrPurple = 105 // inactive tile border
	clrCyan   = 6
	clrBlue   = 4
	clrGray   = 245
)

// renderSection lays out one section's tiles into rows that fit w.
func renderSection(b *strings.Builder, tiles []store.DashboardTile, results map[string]WatchSeries, w int) {
	rows := packRows(tiles, w)
	for ri, row := range rows {
		if ri > 0 {
			b.WriteByte('\n')
		}
		writeRow(b, row, results)
	}
	if len(rows) > 0 {
		b.WriteByte('\n')
	}
}

// packedTile is one tile with its computed render width.
type packedTile struct {
	tile  store.DashboardTile
	width int
}

const gridCols = 4

// tileSpan maps a size hint to a column span in the grid.
func tileSpan(t store.DashboardTile) int {
	switch t.Size {
	case "sm":
		return 1
	case "lg":
		return 3
	default: // md or unset
		return 2
	}
}

// packRows groups tiles into rows using a fixed column grid. Each
// tile occupies 1–4 column spans; rows break when adding the next
// tile would exceed gridCols. Tile widths are computed proportionally
// from the available space (total width minus inter-tile gaps).
// Below singleColumnThreshold everything collapses to one per row.
func packRows(tiles []store.DashboardTile, w int) [][]packedTile {
	if w < singleColumnThreshold {
		rows := make([][]packedTile, len(tiles))
		for i, t := range tiles {
			rows[i] = []packedTile{{tile: t, width: w}}
		}
		return rows
	}

	type spanTile struct {
		tile store.DashboardTile
		span int
	}

	var rows [][]spanTile
	var cur []spanTile
	curSpan := 0
	for _, t := range tiles {
		s := tileSpan(t)
		if s > gridCols {
			s = gridCols
		}
		if curSpan+s > gridCols {
			if len(cur) > 0 {
				rows = append(rows, cur)
			}
			cur = nil
			curSpan = 0
		}
		cur = append(cur, spanTile{tile: t, span: s})
		curSpan += s
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}

	var out [][]packedTile
	for _, row := range rows {
		totalSpan := 0
		for _, st := range row {
			totalSpan += st.span
		}
		available := w - (len(row)-1)*tileGap
		packed := make([]packedTile, len(row))
		assigned := 0
		for i, st := range row {
			if i == len(row)-1 {
				packed[i] = packedTile{tile: st.tile, width: available - assigned}
			} else {
				tw := available * st.span / totalSpan
				packed[i] = packedTile{tile: st.tile, width: tw}
				assigned += tw
			}
		}
		out = append(out, packed)
	}
	return out
}

// writeRow renders one row of tiles side-by-side, each wrapped in a
// box-drawing border. Tiles are padded to the tallest in the row.
func writeRow(b *strings.Builder, row []packedTile, results map[string]WatchSeries) {
	if len(row) == 0 {
		return
	}
	cells := make([][]string, len(row))
	maxLines := 0
	for i, pt := range row {
		// Inner width is tile width minus 2 for the left/right border chars.
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		series, ok := results[resultKey(pt.tile)]
		var content []string
		if !ok {
			content = renderMissing(pt.tile, innerW)
		} else {
			content = renderTile(pt.tile, series, innerW)
		}
		cells[i] = content
		if len(content) > maxLines {
			maxLines = len(content)
		}
	}
	// Pad shorter cells at the TOP so content sits flush at the bottom.
	for i := range cells {
		for len(cells[i]) < maxLines {
			innerW := row[i].width - 2
			if innerW < 4 {
				innerW = 4
			}
			cells[i] = append([]string{strings.Repeat(" ", innerW)}, cells[i]...)
		}
	}
	// Top border with inset title
	for i, pt := range row {
		if i > 0 {
			b.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		label := tileLabel(pt.tile)
		maxLabel := innerW - 4 // room for "─ " + " ─" around label
		if maxLabel < 0 {
			maxLabel = 0
		}
		if len(label) > maxLabel {
			label = label[:maxLabel]
		}
		if label != "" {
			trail := innerW - 2 - len(label) - 1 // ┌─ label ───┐
			if trail < 1 {
				trail = 1
			}
			b.WriteString(colorize(clrPurple, "┌─"))
			b.WriteString(fmt.Sprintf(" \x1b[1;38;5;%dm%s\x1b[0m ", clrOrange, label))
			b.WriteString(colorize(clrPurple, strings.Repeat("─", trail)+"┐"))
		} else {
			b.WriteString(colorize(clrPurple, "┌"+strings.Repeat("─", innerW)+"┐"))
		}
	}
	b.WriteByte('\n')
	// Padding row below top border
	for i, pt := range row {
		if i > 0 {
			b.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		b.WriteString(colorize(clrPurple, "│"))
		b.WriteString(strings.Repeat(" ", innerW))
		b.WriteString(colorize(clrPurple, "│"))
	}
	b.WriteByte('\n')
	// Content rows
	for line := 0; line < maxLines; line++ {
		for i, cell := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", tileGap))
			}
			innerW := row[i].width - 2
			if innerW < 4 {
				innerW = 4
			}
			b.WriteString(colorize(clrPurple, "│"))
			b.WriteString(padRight(cell[line], innerW))
			b.WriteString(colorize(clrPurple, "│"))
		}
		b.WriteByte('\n')
	}
	// Bottom border
	for i, pt := range row {
		if i > 0 {
			b.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		b.WriteString(colorize(clrPurple, "└"+strings.Repeat("─", innerW)+"┘"))
	}
	b.WriteByte('\n')
}

// tileContentHeight returns the expected content height for a tile kind
// so the frame can be pre-allocated before data arrives.
func tileContentHeight(kind string) int {
	switch kind {
	case "counter":
		return bigHeight + 1
	case "sparkline":
		return sparklineHeight
	case "line", "bar":
		return chartHeight
	case "table":
		return 7
	case "gauge":
		return 1
	}
	return 1
}

func renderMissing(t store.DashboardTile, w int) []string {
	h := tileContentHeight(t.Kind)
	out := make([]string, h)
	mid := h / 2
	for i := range out {
		if i == mid {
			out[i] = padRight("  "+colorize(clrGray, "<no data>"), w)
		} else {
			out[i] = strings.Repeat(" ", w)
		}
	}
	return out
}

// renderTile dispatches per-kind rendering.
func renderTile(t store.DashboardTile, series WatchSeries, w int) []string {
	switch t.Kind {
	case "counter":
		return renderCounter(t, series, w)
	case "gauge":
		return []string{renderGauge(t, series, w)}
	case "sparkline", "line", "bar":
		return renderSeries(t, series, w, tileContentHeight(t.Kind))
	case "table":
		return renderTable(t, series, w)
	}
	return []string{padRight("  ?", w)}
}

func renderCounter(t store.DashboardTile, series WatchSeries, w int) []string {
	points, msg := series.points(t)
	if msg != "" {
		return wrapMessage(msg, w)
	}
	v, ok := headline(points)
	if !ok {
		return renderMissing(t, w)
	}
	d := store.FormatTileDisplay(t, &v)
	text := d.Before + d.Num + d.After
	color := alertColor(series, v)
	lines := bigNumber(text)
	var out []string
	for _, line := range lines {
		contentW := visualLen(line)
		lpad := (w - contentW) / 2
		if lpad < 0 {
			lpad = 0
		}
		display := strings.Repeat(" ", lpad) + colorize(color, line)
		out = append(out, padRight(display, w))
	}
	out = append(out, strings.Repeat(" ", w))
	return out
}

func renderGauge(t store.DashboardTile, series WatchSeries, w int) string {
	points, msg := series.points(t)
	if msg != "" {
		if len(msg) > w-3 {
			msg = msg[:max(w-3, 0)] + "…"
		}
		return padRight("  "+colorize(clrGray, msg), w)
	}
	v, ok := headline(points)
	if !ok {
		return padRight("  "+colorize(clrGray, "<no data>"), w) // gauge is 1 line; no padding needed
	}
	max := 100.0
	if t.Max != nil {
		max = *t.Max
	}
	pct := 0.0
	if max > 0 {
		pct = v / max
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	d := store.FormatTileDisplay(t, &v)
	suffix := " " + d.Num + d.After
	barW := w - 2 - len(suffix) - 2 // -2 for []
	if barW < 4 {
		barW = 4
	}
	filled := int(float64(barW) * pct)
	color := alertColor(series, v)
	bar := "[" + colorize(color, strings.Repeat("█", filled)) + strings.Repeat("░", barW-filled) + "]"
	display := "  " + bar + suffix
	return padRight(display, w)
}

const (
	sparklineHeight = 4
	chartHeight     = 8
)

// renderSeries draws a line, sparkline or bar tile exactly as `chart <cron>`
// would with the tile's aggregate and bucket. A sparkline tile is a short
// line chart, not chart's one-line glyph.
func renderSeries(t store.DashboardTile, series WatchSeries, w, h int) []string {
	if len(series.Series.Buckets) == 0 {
		return renderMissing(t, w)
	}
	args := t.ChartArgs()
	if args.Kind == "sparkline" {
		args.Kind = "line"
	}
	rendered, msg := weftchart.RenderCronTitled(t.Watch, series.Series, args, "", w, h)
	if msg != "" {
		return wrapMessage(msg, w)
	}
	raw := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	out := make([]string, len(raw))
	for i, line := range raw {
		out[i] = padRight(line, w)
	}
	return out
}

// wrapMessage lays a chart error out in gray, word-wrapped to w.
func wrapMessage(msg string, w int) []string {
	var out []string
	line := " "
	for _, word := range strings.Fields(msg) {
		if len(line) > 1 && len(line)+1+len(word) > w {
			out = append(out, padRight(colorize(clrGray, line), w))
			line = " "
		}
		line += " " + word
	}
	return append(out, padRight(colorize(clrGray, line), w))
}

// tableRows lays out a cron's latest run as a table: a tag column and a
// value column, or the value alone for an untagged cron. Rows are already
// in table order (numbers highest first, text by tag).
func tableRows(latest []weftchart.LatestPoint) (headers []string, rows [][]string) {
	tagged := false
	for _, p := range latest {
		tagged = tagged || p.Tag != ""
	}
	if tagged {
		headers = []string{"tag", "value"}
	} else {
		headers = []string{"value"}
	}
	for _, p := range latest {
		if tagged {
			rows = append(rows, []string{p.Tag, p.Display()})
		} else {
			rows = append(rows, []string{p.Display()})
		}
	}
	return headers, rows
}

func renderTable(t store.DashboardTile, series WatchSeries, w int) []string {
	points, msg := series.points(t)
	if msg != "" {
		return wrapMessage(msg, w)
	}
	if len(points) == 0 {
		return renderMissing(t, w)
	}
	headers, rows := tableRows(points)
	colW := max((w-2)/len(headers), 4)
	out := []string{padRight("  "+colorize(clrBlue, joinCols(headers, colW)), w)}
	for i, row := range rows {
		line := "  " + joinCols(row, colW)
		if points[i].Value != nil && series.Series.Alert.Breached(*points[i].Value) {
			line = colorize(1, line)
		}
		out = append(out, padRight(line, w))
		if len(out) > 6 {
			break
		}
	}
	return out
}

func tileLabel(t store.DashboardTile) string {
	if t.Title != "" {
		return t.Title
	}
	return t.Watch
}

func joinCols(cols []string, colW int) string {
	var parts []string
	for _, c := range cols {
		if len(c) > colW-1 {
			c = c[:colW-1]
		}
		parts = append(parts, padRight(c, colW))
	}
	return strings.Join(parts, "")
}

// alertColor is red when v breaches the cron's alert cutoff, the only
// threshold a tile has, and the default color otherwise.
func alertColor(series WatchSeries, v float64) int {
	if series.Series.Alert.Breached(v) {
		return 1 // red
	}
	return clrOrange
}

// colorize wraps s in an ANSI 256-color sequence. color=0 means no color.
func colorize(color int, s string) string {
	if color == 0 {
		return s
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", color, s)
}

func sectionHeading(title string, w int) string {
	rule := colorize(clrGray, strings.Repeat("─", w-len(title)))
	return fmt.Sprintf("\x1b[1;38;5;%dm%s\x1b[0m\n%s", clrBlue, title, rule)
}

func boldTitle(title string) string {
	return fmt.Sprintf("\x1b[1;38;5;%dm%s\x1b[0m", clrCyan, title)
}

// padRight pads s with spaces so its rendered width is `w` columns,
// accounting for any ANSI escapes that don't take visual width.
// Caller is responsible for accounting for ANSI when sizing.
func padRight(s string, w int) string {
	vis := visualLen(s)
	if vis >= w {
		return s
	}
	return s + strings.Repeat(" ", w-vis)
}

// visualLen counts visible columns, ignoring ANSI escape sequences.
// Treats every rune as 1 column (good enough for ASCII + the block
// runes used by sparkline). The chart sparkline runes ▁..█ are all
// 1 column wide in standard terminals.
func visualLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

const bigHeight = 5

// bigNumber renders a numeric string as 5-line tall block art.
func bigNumber(s string) []string {
	lines := [bigHeight]strings.Builder{}
	for _, ch := range s {
		glyph, ok := bigDigits[ch]
		if !ok {
			glyph = bigDigits['?']
		}
		for row := 0; row < bigHeight; row++ {
			if lines[row].Len() > 0 {
				lines[row].WriteByte(' ')
			}
			lines[row].WriteString(glyph[row])
		}
	}
	out := make([]string, bigHeight)
	for i := range out {
		out[i] = lines[i].String()
	}
	return out
}

var bigDigits = map[rune][bigHeight]string{
	'0': {"█▀▀█", "█  █", "█  █", "█  █", "█▄▄█"},
	'1': {" ▄█ ", "  █ ", "  █ ", "  █ ", "  █ "},
	'2': {"█▀▀█", "   █", "▄▄▄█", "█   ", "█▄▄▄"},
	'3': {"█▀▀█", "   █", " ▀▀█", "   █", "█▄▄█"},
	'4': {"█  █", "█  █", "▀▀▀█", "   █", "   █"},
	'5': {"█▀▀▀", "█   ", "█▀▀█", "   █", "█▄▄█"},
	'6': {"█▀▀▀", "█   ", "█▀▀█", "█  █", "█▄▄█"},
	'7': {"▀▀▀█", "   █", "   █", "  █ ", "  █ "},
	'8': {"█▀▀█", "█  █", "█▀▀█", "█  █", "█▄▄█"},
	'9': {"█▀▀█", "█  █", "▀▀▀█", "   █", "█▄▄█"},
	'.': {"    ", "    ", "    ", "    ", " ▄  "},
	',': {"    ", "    ", "    ", " ▄  ", " ▀  "},
	'-': {"    ", "    ", "▀▀▀▀", "    ", "    "},
	'?': {"    ", " ▄▄ ", "  █ ", "    ", "  ▄ "},
}
