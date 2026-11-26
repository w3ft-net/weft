package dashview

import (
	"fmt"
	"strings"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	weftchart "github.com/w3ft-net/weft/chart"
	dspec "github.com/w3ft-net/weft/dashboard"
)

const viewName = "dashboard"

// ViewArgs is passed to OnEnter when the dashboard view is activated.
// Results may be nil; the view renders the frame immediately and fills
// tile data asynchronously.
type ViewArgs struct {
	Name    string // dashboard name (used as fallback title)
	Spec    *dspec.Spec
	Results map[string]WatchSeries // nil = fetch async
}

// DashboardView implements shellcore.View for interactive dashboard
// navigation. Arrow keys move focus between tiles; Enter zooms the
// focused tile to full screen; Esc exits zoom or the view entirely.
type DashboardView struct {
	name     string // dashboard name
	spec     *dspec.Spec
	results  map[string]WatchSeries
	provider Provider

	tiles    []dspec.Tile
	focus    int
	zoom     bool
	scroll   int   // line offset into the rendered grid
	lastTick int64 // unix seconds of last refresh

	pending chan map[string]WatchSeries // async data delivery (cap 1)
}

func NewView(p Provider) *DashboardView {
	return &DashboardView{provider: p}
}

func (v *DashboardView) Name() string { return viewName }

func (v *DashboardView) OnEnter(c *shellcore.Shell, args any) error {
	va, ok := args.(*ViewArgs)
	if !ok {
		return fmt.Errorf("dashboard: invalid args")
	}
	v.name = va.Name
	v.spec = va.Spec
	v.results = va.Results
	v.focus = 0
	v.zoom = false
	v.scroll = 0
	v.lastTick = time.Now().Unix()

	v.tiles = nil
	for _, sec := range v.spec.Sections {
		v.tiles = append(v.tiles, sec.Tiles...)
	}
	if len(v.tiles) == 0 {
		return fmt.Errorf("dashboard: no tiles")
	}

	c.Write([]byte("\x1b[?1049h"))

	if v.results == nil {
		v.pending = make(chan map[string]WatchSeries, 1)
		go func() {
			v.pending <- fetchAllSeries(v.provider, v.spec)
		}()
	}
	return nil
}

func (v *DashboardView) OnExit(c *shellcore.Shell) {
	c.Write([]byte("\x1b[?1049l"))
}

func (v *DashboardView) HandleKey(c *shellcore.Shell, ev shellcore.KeyEvent) bool {
	if v.zoom {
		if ev.Key == shellcore.KeyEsc {
			v.zoom = false
			v.Render(c)
			return true
		}
		return true
	}

	switch ev.Key {
	case shellcore.KeyEsc:
		c.ExitView()
		return true
	case shellcore.KeyEnter:
		v.zoom = true
		v.Render(c)
		return true
	case shellcore.KeyLeft:
		if v.focus > 0 {
			v.focus--
			v.Render(c)
		}
		return true
	case shellcore.KeyRight:
		if v.focus < len(v.tiles)-1 {
			v.focus++
			v.Render(c)
		}
		return true
	case shellcore.KeyUp:
		v.moveVertical(c, -1)
		return true
	case shellcore.KeyDown:
		v.moveVertical(c, 1)
		return true
	}

	if ev.Rune == 'q' || ev.Rune == 'Q' {
		c.ExitView()
		return true
	}
	return true
}

func (v *DashboardView) moveVertical(c *shellcore.Shell, dir int) {
	w := c.Width()
	if w < singleColumnThreshold {
		v.focus += dir
		v.clampFocus()
		v.Render(c)
		return
	}

	rows := v.tileRows()
	curRow, curCol := v.focusRowCol(rows)
	targetRow := curRow + dir
	if targetRow < 0 || targetRow >= len(rows) {
		return
	}
	target := rows[targetRow].start + curCol
	if curCol >= rows[targetRow].count {
		target = rows[targetRow].start + rows[targetRow].count - 1
	}
	v.focus = target
	v.clampFocus()
	v.Render(c)
}

type tileRow struct {
	start int
	count int
}

func (v *DashboardView) tileRows() []tileRow {
	var rows []tileRow
	idx := 0
	for _, sec := range v.spec.Sections {
		tiles := sec.Tiles
		for len(tiles) > 0 {
			span := 0
			count := 0
			for _, t := range tiles {
				s := tileSpan(t)
				if span+s > gridCols {
					break
				}
				span += s
				count++
			}
			if count == 0 {
				count = 1
			}
			rows = append(rows, tileRow{start: idx, count: count})
			idx += count
			tiles = tiles[count:]
		}
	}
	return rows
}

func (v *DashboardView) focusRowCol(rows []tileRow) (int, int) {
	for ri, r := range rows {
		if v.focus >= r.start && v.focus < r.start+r.count {
			return ri, v.focus - r.start
		}
	}
	return 0, 0
}

func (v *DashboardView) clampFocus() {
	if v.focus < 0 {
		v.focus = 0
	}
	if v.focus >= len(v.tiles) {
		v.focus = len(v.tiles) - 1
	}
}

func (v *DashboardView) Render(c *shellcore.Shell) {
	w := c.Width()
	h := c.Height()
	if w < 20 {
		w = 20
	}

	var buf strings.Builder
	buf.WriteString("\x1b[?2026h") // DEC sync update begin
	buf.WriteString("\x1b[H\x1b[2J")

	if v.zoom {
		v.renderZoomed(&buf, w, h)
	} else {
		v.renderGrid(&buf, w, h)
	}

	buf.WriteString("\x1b[?2026l") // DEC sync update end
	c.Write([]byte(buf.String()))
}

func (v *DashboardView) renderGrid(buf *strings.Builder, w, h int) {
	// Render all grid lines into a slice, tracking which lines belong
	// to each tile so we can scroll to keep focus visible.
	var lines []string
	tileIdx := 0
	// tileLineStart[i] = first line index for tile i
	tileLineStart := make(map[int]int)
	tileLineEnd := make(map[int]int)

	title := v.spec.Title
	if title == "" {
		title = v.name
	}
	lines = append(lines, boldTitle(title), "")

	for si, sec := range v.spec.Sections {
		if si > 0 {
			lines = append(lines, "")
		}
		if sec.Title != "" {
			lines = append(lines, sectionHeading(sec.Title, w), "")
		}
		packed := packRows(sec.Tiles, w)
		for ri, row := range packed {
			if ri > 0 {
				lines = append(lines, "")
			}
			startLine := len(lines)
			rowLines := v.renderRowLines(row, tileIdx, w)
			lines = append(lines, rowLines...)
			endLine := len(lines)
			for i := range row {
				tileLineStart[tileIdx+i] = startLine
				tileLineEnd[tileIdx+i] = endLine
			}
			tileIdx += len(row)
		}
	}

	// Viewport is h-1 lines (reserve bottom row for status bar).
	viewport := h - 1
	if viewport < 1 {
		viewport = 1
	}

	// Scroll to keep focused tile visible.
	if start, ok := tileLineStart[v.focus]; ok {
		end := tileLineEnd[v.focus]
		if start < v.scroll {
			v.scroll = start
		}
		if end > v.scroll+viewport {
			v.scroll = end - viewport
		}
	}
	if v.scroll < 0 {
		v.scroll = 0
	}
	maxScroll := len(lines) - viewport
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.scroll > maxScroll {
		v.scroll = maxScroll
	}

	// Output the visible slice.
	end := v.scroll + viewport
	if end > len(lines) {
		end = len(lines)
	}
	for _, line := range lines[v.scroll:end] {
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}

	// Status bar pinned to bottom row
	buf.WriteString(fmt.Sprintf("\x1b[%d;1H", h))
	buf.WriteString("\x1b[7m")
	status := " \xe2\x86\x91\xe2\x86\x93\xe2\x86\x90\xe2\x86\x92 navigate  \xe2\x8f\x8e zoom  esc quit "
	buf.WriteString(padRight(status, w))
	buf.WriteString("\x1b[0m")
}

// renderRowLines returns the rendered lines for one grid row of tiles.
func (v *DashboardView) renderRowLines(row []packedTile, startIdx int, w int) []string {
	if len(row) == 0 {
		return nil
	}

	cells := make([][]string, len(row))
	maxLines := 0
	for i, pt := range row {
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		series, ok := v.results[resultKey(pt.tile)]
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
	for i := range cells {
		innerW := row[i].width - 2
		if innerW < 4 {
			innerW = 4
		}
		for len(cells[i]) < maxLines {
			cells[i] = append([]string{strings.Repeat(" ", innerW)}, cells[i]...)
		}
	}

	var out []string

	// Top border with inset title
	var topBorder strings.Builder
	for i, pt := range row {
		if i > 0 {
			topBorder.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		bclr := v.borderColor(startIdx + i)
		label := tileLabel(pt.tile)
		maxLabel := innerW - 4
		if maxLabel < 0 {
			maxLabel = 0
		}
		if len(label) > maxLabel {
			label = label[:maxLabel]
		}
		if label != "" {
			trail := innerW - 2 - len(label) - 1
			if trail < 1 {
				trail = 1
			}
			topBorder.WriteString(colorize(bclr, "┌─"))
			topBorder.WriteString(fmt.Sprintf(" \x1b[1;38;5;%dm%s\x1b[0m ", clrOrange, label))
			topBorder.WriteString(colorize(bclr, strings.Repeat("─", trail)+"┐"))
		} else {
			topBorder.WriteString(colorize(bclr, "┌"+strings.Repeat("─", innerW)+"┐"))
		}
	}
	out = append(out, topBorder.String())

	// Padding row
	var padRow strings.Builder
	for i, pt := range row {
		if i > 0 {
			padRow.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		bclr := v.borderColor(startIdx + i)
		padRow.WriteString(colorize(bclr, "│"))
		padRow.WriteString(strings.Repeat(" ", innerW))
		padRow.WriteString(colorize(bclr, "│"))
	}
	out = append(out, padRow.String())

	// Content rows
	for line := 0; line < maxLines; line++ {
		var lb strings.Builder
		for i, cell := range cells {
			if i > 0 {
				lb.WriteString(strings.Repeat(" ", tileGap))
			}
			innerW := row[i].width - 2
			if innerW < 4 {
				innerW = 4
			}
			bclr := v.borderColor(startIdx + i)
			lb.WriteString(colorize(bclr, "│"))
			lb.WriteString(padRight(cell[line], innerW))
			lb.WriteString(colorize(bclr, "│"))
		}
		out = append(out, lb.String())
	}

	// Bottom border
	var botBorder strings.Builder
	for i, pt := range row {
		if i > 0 {
			botBorder.WriteString(strings.Repeat(" ", tileGap))
		}
		innerW := pt.width - 2
		if innerW < 4 {
			innerW = 4
		}
		bclr := v.borderColor(startIdx + i)
		botBorder.WriteString(colorize(bclr, "└"+strings.Repeat("─", innerW)+"┘"))
	}
	out = append(out, botBorder.String())

	return out
}

func (v *DashboardView) borderColor(idx int) int {
	if idx == v.focus {
		return clrPeach
	}
	return clrPurple
}

func (v *DashboardView) renderZoomed(buf *strings.Builder, w, h int) {
	t := v.tiles[v.focus]
	series := v.results[resultKey(t)]

	// Use the chart library for a proper full-screen chart render,
	// same as `chart <watch>` does.
	chartH := h - 2 // reserve title row + status bar
	if chartH < 5 {
		chartH = 5
	}

	var body string
	switch {
	case len(series.Series.Buckets) == 0 && len(series.Latest) == 0:
		body = shellcore.DimText(fmt.Sprintf("(no data for %q)", t.Watch))
	case t.Kind == "table":
		body = v.renderZoomedTable(t, series, w, chartH)
	default:
		body = v.renderZoomedChart(t, series, w, chartH)
	}

	// Write the chart body with \r\n
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}

	// Status bar pinned to bottom
	buf.WriteString(fmt.Sprintf("\x1b[%d;1H", h))
	buf.WriteString("\x1b[7m")
	status := fmt.Sprintf(" %s  esc back ", t.Watch)
	buf.WriteString(padRight(status, w))
	buf.WriteString("\x1b[0m")
}

func (v *DashboardView) renderZoomedChart(t dspec.Tile, series WatchSeries, w, h int) string {
	if len(series.Series.Buckets) == 0 {
		return shellcore.DimText(fmt.Sprintf("(no numeric data for %q)", t.Watch))
	}
	// A counter or gauge zooms to its history as a line.
	args := t.ChartArgs()
	if !t.DrawsSeries() || args.Kind == "sparkline" {
		args.Kind = "line"
	}
	rendered, msg := weftchart.RenderCronTitled(t.Watch, series.Series, args, t.Watch, w, h)
	if msg != "" {
		return shellcore.ErrorText(msg)
	}
	return rendered
}

func (v *DashboardView) renderZoomedTable(t dspec.Tile, series WatchSeries, w, h int) string {
	points, msg := series.points(t)
	if msg != "" {
		return shellcore.ErrorText(msg)
	}
	if len(points) == 0 {
		return shellcore.DimText("(no data)")
	}
	var buf strings.Builder
	buf.WriteString("\x1b[1m")
	buf.WriteString(t.Watch)
	buf.WriteString("\x1b[0m\n\n")

	headers, rows := tableRows(points)
	colW := max((w-2)/len(headers), 6)

	// Header row
	buf.WriteString(colorize(clrBlue, joinCols(headers, colW)))
	buf.WriteString("\n")
	buf.WriteString(strings.Repeat("─", colW*len(headers)))
	buf.WriteString("\n")

	for i, row := range rows {
		line := joinCols(row, colW)
		if points[i].Value != nil && series.Series.Alert.Breached(*points[i].Value) {
			line = colorize(1, line)
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return buf.String()
}

const refreshInterval = 30 // seconds between live data refreshes

func (v *DashboardView) Tick(c *shellcore.Shell) {
	// Check for async data from initial load.
	if v.pending != nil {
		select {
		case data := <-v.pending:
			v.results = data
			v.pending = nil
			v.lastTick = time.Now().Unix()
			v.Render(c)
			return
		default:
		}
	}

	if v.zoom {
		return
	}
	now := time.Now().Unix()
	if now-v.lastTick < refreshInterval {
		return
	}
	v.lastTick = now
	v.results = fetchAllSeries(v.provider, v.spec)
	v.Render(c)
}
