// Package dashboard renders a parsed store.DashboardSpec into ANSI text
// sized to a terminal viewport. Pure rendering — fetching the spec and
// the per-watch results is the caller's job (see Builtin for the shell
// wiring that bundles both).
package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore/chart"
	"github.com/graybeardcorp/weft/store"
)

// WatchSeries is the data a tile needs to render: the latest sample
// (for counter/gauge), the chronological series (for sparkline), and
// the most recent stdout (for table). Newest sample is index 0; the
// renderer reverses for left-to-right time when needed.
//
// Tags is populated instead of Values/Times for a cron that stores
// tags — one sub-series per tag (the smallest folded into "other").
// See toWatchSeries.
type WatchSeries struct {
	Values     []float64 // numeric values, newest-first
	Times      []float64 // epoch seconds, parallel to Values, newest-first
	LatestText string    // raw stdout of the most recent run (for table)
	Tags       map[string]WatchSeries
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
		series, ok := results[pt.tile.Watch]
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
	case "sparkline":
		return renderSparkline(series, w)
	case "table":
		return renderTable(t, series, w)
	}
	return []string{padRight("  ?", w)}
}

func renderCounter(t store.DashboardTile, series WatchSeries, w int) []string {
	if len(series.Values) == 0 {
		return renderMissing(t, w)
	}
	v := series.Values[0]
	d := store.FormatTileDisplay(t, &v)
	text := d.Before + d.Num + d.After
	color := thresholdColor(t.Threshold, v)
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
	if len(series.Values) == 0 {
		return padRight("  "+colorize(clrGray, "<no data>"), w) // gauge is 1 line; no padding needed
	}
	v := series.Values[0]
	max := 100.0
	if t.Threshold != nil && t.Threshold.Max != nil {
		max = *t.Threshold.Max
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
	color := thresholdColor(t.Threshold, v)
	bar := "[" + colorize(color, strings.Repeat("█", filled)) + strings.Repeat("░", barW-filled) + "]"
	display := "  " + bar + suffix
	return padRight(display, w)
}

const sparklineHeight = 4

func renderSparkline(series WatchSeries, w int) []string {
	if len(series.Tags) > 0 {
		return renderSparklineTagged(series, w)
	}
	if len(series.Values) == 0 {
		return renderMissing(store.DashboardTile{Kind: "sparkline"}, w)
	}
	pts, hasTimes := seriesToPoints(series)
	b := chart.Line(w, sparklineHeight).Points(pts)
	if hasTimes {
		b = b.XAxisTime()
	}
	rendered, err := b.Render()
	if err != nil {
		return []string{padRight("  "+colorize(clrGray, "<render error>"), w)}
	}
	raw := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	out := make([]string, len(raw))
	for i, line := range raw {
		out[i] = padRight(colorize(clrCyan, line), w)
	}
	return out
}

// renderSparklineTagged is renderSparkline for a tagged cron: one
// warp chart.NamedSeries per distinct tag value, sharing one x/y scale
// (warp's Builder.Series already handles per-series color + legend).
// Unlike the single-series path, lines are NOT force-colorized here —
// the builder already emits an explicit per-series color, and wrapping
// the whole rendered block in one outer color would fight that.
func renderSparklineTagged(series WatchSeries, w int) []string {
	if len(series.Tags) == 0 {
		return renderMissing(store.DashboardTile{Kind: "sparkline"}, w)
	}
	tags := make([]string, 0, len(series.Tags))
	for tag := range series.Tags {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	named := make([]chart.NamedSeries, 0, len(tags))
	hasTimes := false
	for _, tag := range tags {
		pts, tagHasTimes := seriesToPoints(series.Tags[tag])
		hasTimes = hasTimes || tagHasTimes
		named = append(named, chart.NamedSeries{Label: tag, Points: pts})
	}

	b := chart.Line(w, sparklineHeight).Series(named)
	if hasTimes {
		b = b.XAxisTime()
	}
	rendered, err := b.Render()
	if err != nil {
		return []string{padRight("  "+colorize(clrGray, "<render error>"), w)}
	}
	raw := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	out := make([]string, len(raw))
	for i, line := range raw {
		out[i] = padRight(line, w)
	}
	return out
}

// seriesToPoints converts a WatchSeries' newest-first Values/Times into
// oldest-first chart.Points, the shape both the single- and tagged-
// sparkline renderers need.
func seriesToPoints(series WatchSeries) (pts []chart.Point, hasTimes bool) {
	n := len(series.Values)
	hasTimes = len(series.Times) == n && n > 0
	pts = make([]chart.Point, n)
	for i := range series.Values {
		ri := n - 1 - i // oldest-first
		x := float64(i)
		if hasTimes {
			x = series.Times[ri]
		}
		pts[i] = chart.Point{X: x, Y: series.Values[ri]}
	}
	return pts, hasTimes
}

func renderTable(t store.DashboardTile, series WatchSeries, w int) []string {
	if series.LatestText == "" {
		return renderMissing(t, w)
	}
	headers := t.Columns
	colCount := len(headers)
	if colCount == 0 {
		return renderMissing(t, w)
	}
	colW := (w - 2) / colCount
	if colW < 4 {
		colW = 4
	}
	headerLine := "  " + colorize(clrBlue, joinCols(headers, colW))
	out := []string{padRight(headerLine, w)}
	for _, line := range strings.Split(strings.TrimSpace(series.LatestText), "\n") {
		fields := strings.Fields(line)
		out = append(out, padRight("  "+joinCols(fields, colW), w))
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

func thresholdColor(th *store.DashboardThresh, v float64) int {
	if th == nil {
		return clrOrange
	}
	if th.Crit != nil && v >= *th.Crit {
		return 1 // red
	}
	if th.Warn != nil && v >= *th.Warn {
		return 3 // yellow
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
