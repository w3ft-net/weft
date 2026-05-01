// Package dashboard renders a parsed store.DashboardSpec into ANSI text
// sized to a terminal viewport. Pure rendering — fetching the spec and
// the per-watch results is the caller's job (see Builtin for the shell
// wiring that bundles both).
package dashboard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore/chart"
	"github.com/kevlarcon/weft/store"
)

// WatchSeries is the data a tile needs to render: the latest sample
// (for counter/gauge), the chronological series (for sparkline), and
// the most recent stdout (for table). Newest sample is index 0; the
// renderer reverses for left-to-right time when needed.
type WatchSeries struct {
	Values     []float64 // numeric values, newest-first
	LatestText string    // raw stdout of the most recent run (for table)
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

// packRows greedily groups tiles into rows that fit w columns, taking
// per-tile size hints + kind minimums into account. Below
// singleColumnThreshold the layout collapses to one tile per row at
// full width.
func packRows(tiles []store.DashboardTile, w int) [][]packedTile {
	if w < singleColumnThreshold {
		rows := make([][]packedTile, len(tiles))
		for i, t := range tiles {
			rows[i] = []packedTile{{tile: t, width: w}}
		}
		return rows
	}

	var rows [][]packedTile
	var cur []packedTile
	curW := 0
	for _, t := range tiles {
		preferred := preferredWidth(t, w)
		min := minTileWidth(t)
		need := preferred
		if len(cur) > 0 {
			need += tileGap
		}
		if curW+need > w {
			// Flush current row, start new with this tile.
			if len(cur) > 0 {
				rows = append(rows, cur)
				cur = nil
				curW = 0
			}
			// Use full width if this tile alone wouldn't have fit otherwise.
			width := preferred
			if width > w {
				width = w
			}
			if width < min {
				width = min
			}
			cur = []packedTile{{tile: t, width: width}}
			curW = width
			continue
		}
		cur = append(cur, packedTile{tile: t, width: preferred})
		curW += need
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	return rows
}

// preferredWidth maps the tile's size hint onto the available row
// width. Defaults to "md" when unset.
func preferredWidth(t store.DashboardTile, rowW int) int {
	size := t.Size
	if size == "" {
		size = "md"
	}
	var frac float64
	switch size {
	case "sm":
		frac = 0.25
	case "lg":
		frac = 0.66
	default: // md
		frac = 0.5
	}
	wOut := int(float64(rowW) * frac)
	if wOut < minTileWidth(t) {
		wOut = minTileWidth(t)
	}
	if wOut > rowW {
		wOut = rowW
	}
	return wOut
}

func minTileWidth(t store.DashboardTile) int {
	switch t.Kind {
	case "counter":
		return 14
	case "gauge":
		return 18
	case "sparkline":
		return 24
	case "table":
		return 22
	}
	return 16
}

// writeRow renders one row of tiles side-by-side. Each tile becomes a
// list of lines; we pad to the tallest tile and concat line-wise.
func writeRow(b *strings.Builder, row []packedTile, results map[string]WatchSeries) {
	if len(row) == 0 {
		return
	}
	cells := make([][]string, len(row))
	maxLines := 0
	for i, pt := range row {
		series, ok := results[pt.tile.Watch]
		var content []string
		if !ok {
			content = renderMissing(pt.tile, pt.width)
		} else {
			content = renderTile(pt.tile, series, pt.width)
		}
		cells[i] = content
		if len(content) > maxLines {
			maxLines = len(content)
		}
	}
	for i := range cells {
		for len(cells[i]) < maxLines {
			cells[i] = append(cells[i], strings.Repeat(" ", row[i].width))
		}
	}
	for line := 0; line < maxLines; line++ {
		for i, cell := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", tileGap))
			}
			b.WriteString(padRight(cell[line], row[i].width))
		}
		b.WriteByte('\n')
	}
}

func renderMissing(t store.DashboardTile, w int) []string {
	return []string{
		tileHeader(t, w),
		padRight("  <no data>", w),
	}
}

// renderTile dispatches per-kind rendering. All renderings have the
// header line + one-or-more body lines.
func renderTile(t store.DashboardTile, series WatchSeries, w int) []string {
	header := tileHeader(t, w)
	switch t.Kind {
	case "counter":
		return []string{header, renderCounter(t, series, w)}
	case "gauge":
		return []string{header, renderGauge(t, series, w)}
	case "sparkline":
		return []string{header, renderSparkline(series, w)}
	case "table":
		out := []string{header}
		out = append(out, renderTable(t, series, w)...)
		return out
	}
	return []string{header, padRight("  ?", w)}
}

func tileHeader(t store.DashboardTile, w int) string {
	label := t.Watch
	if t.Kind != "" {
		label = label + "  (" + t.Kind + ")"
	}
	if visualLen(label) > w-2 {
		label = label[:w-2]
	}
	return padRight("\x1b[1m"+label+"\x1b[0m", w+len("\x1b[1m\x1b[0m"))
}

func renderCounter(t store.DashboardTile, series WatchSeries, w int) string {
	if len(series.Values) == 0 {
		return padRight("  <no data>", w)
	}
	v := series.Values[0]
	text := formatNumber(v)
	if t.Format != "" {
		text = fmt.Sprintf(t.Format, v)
	}
	color := thresholdColor(t.Threshold, v)
	display := "  " + colorize(color, text)
	return padRight(display, w+colorPad(color))
}

func renderGauge(t store.DashboardTile, series WatchSeries, w int) string {
	if len(series.Values) == 0 {
		return padRight("  <no data>", w)
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
	// Gauge bar: width = w - 2 (margin) - len(value text) - len("/max ")
	suffix := fmt.Sprintf(" %s/%s", formatNumber(v), formatNumber(max))
	barW := w - 2 - len(suffix)
	if barW < 4 {
		barW = 4
	}
	filled := int(float64(barW) * pct)
	color := thresholdColor(t.Threshold, v)
	bar := "[" + colorize(color, strings.Repeat("█", filled)) + strings.Repeat("░", barW-filled) + "]"
	display := "  " + bar + suffix
	return padRight(display, w+colorPad(color))
}

func renderSparkline(series WatchSeries, w int) string {
	if len(series.Values) == 0 {
		return padRight("  <no data>", w)
	}
	// Reverse to chronological order, downsample to width-2.
	target := w - 2
	if target < 2 {
		target = 2
	}
	chrono := make([]float64, len(series.Values))
	for i, v := range series.Values {
		chrono[len(series.Values)-1-i] = v
	}
	if len(chrono) > target {
		chrono = chrono[len(chrono)-target:]
	}
	spark := chart.Sparkline(chrono)
	display := "  " + spark
	return padRight(display, w+visualLen(spark)-runeCount(spark))
}

func renderTable(t store.DashboardTile, series WatchSeries, w int) []string {
	if series.LatestText == "" {
		return []string{padRight("  <no data>", w)}
	}
	headers := t.Columns
	colCount := len(headers)
	if colCount == 0 {
		return []string{padRight("  (no columns)", w)}
	}
	colW := (w - 2) / colCount
	if colW < 4 {
		colW = 4
	}
	out := []string{padRight("  "+joinCols(headers, colW), w)}
	for _, line := range strings.Split(strings.TrimSpace(series.LatestText), "\n") {
		fields := strings.Fields(line)
		out = append(out, padRight("  "+joinCols(fields, colW), w))
		if len(out) > 6 {
			break
		}
	}
	return out
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
		return 0
	}
	if th.Crit != nil && v >= *th.Crit {
		return 1 // red
	}
	if th.Warn != nil && v >= *th.Warn {
		return 3 // yellow
	}
	return 0
}

// colorize wraps s in an ANSI 256-color sequence. color=0 means no color.
func colorize(color int, s string) string {
	if color == 0 {
		return s
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", color, s)
}

// colorPad returns the byte overhead of the color sequence so padRight
// can extend the field properly when the visible content is shorter.
func colorPad(color int) int {
	if color == 0 {
		return 0
	}
	return len(fmt.Sprintf("\x1b[38;5;%dm\x1b[0m", color))
}

func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func sectionHeading(title string, w int) string {
	rule := strings.Repeat("─", w)
	return "\x1b[1m" + title + "\x1b[0m\n" + rule
}

func boldTitle(title string) string {
	return "\x1b[1m" + title + "\x1b[0m"
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
