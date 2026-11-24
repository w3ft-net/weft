package termchart

import (
	"fmt"
	"math"
	"time"
)

// Box-drawing characters for axis frames.
const (
	axisVertical   = '│'
	axisHorizontal = '─'
	axisTickLeft   = '┤'
	axisTickDown   = '┬'
	axisCornerBL   = '└'
)

// Layout reserves space at the top (title), left (Y-axis labels), and
// bottom (X-axis labels) of a chart canvas. The remaining rectangle is
// the plot area where lines / bars get drawn.
const (
	yAxisWidth   = 10
	xAxisHeight  = 2
	titleHeight  = 1
	legendHeight = 1
)

type rect struct {
	x, y, w, h int
}

func (r rect) right() int  { return r.x + r.w }
func (r rect) bottom() int { return r.y + r.h }

type layout struct {
	plot      rect
	yAxis     rect
	xAxis     rect
	title     rect
	legend    rect
	hasTitle  bool
	hasLegend bool
}

// computeLayout reserves space at the top (title), left (Y-axis labels),
// bottom (X-axis labels), and — when withLegend — one more row at the
// very bottom for a series legend. The remaining rectangle is the plot
// area where lines / bars get drawn.
func computeLayout(totalW, totalH int, withTitle, withAxis, withLegend bool) layout {
	var top, left, bottomReserve int
	var l layout

	if withTitle && totalH > titleHeight {
		l.title = rect{0, 0, totalW, titleHeight}
		l.hasTitle = true
		top = titleHeight
	}

	legendH := 0
	if withLegend && totalH-top > legendHeight {
		legendH = legendHeight
		l.hasLegend = true
	}

	if withAxis {
		yw := yAxisWidth
		if yw > totalW {
			yw = totalW
		}
		left = yw
		xh := xAxisHeight
		if xh > totalH-top-legendH {
			xh = totalH - top - legendH
			if xh < 0 {
				xh = 0
			}
		}
		bottomReserve = xh
		l.yAxis = rect{0, top, yw, totalH - top - xh - legendH}
		l.xAxis = rect{left, totalH - legendH - xh, totalW - left, xh}
	}

	if l.hasLegend {
		l.legend = rect{0, totalH - legendH, totalW, legendH}
	}

	l.plot = rect{left, top, totalW - left, totalH - top - bottomReserve - legendH}
	return l
}

// niceTicks returns evenly-spaced "nice" values inside [vmin, vmax],
// targeting at most maxTicks marks. Step is rounded to 1, 2, or 5 ×
// 10^n so labels read naturally (10, 20, 50, 100, …).
func niceTicks(vmin, vmax float64, maxTicks int) []float64 {
	if maxTicks < 2 {
		return nil
	}
	span := vmax - vmin
	if !(span > 0) || math.IsInf(span, 0) || math.IsNaN(span) {
		return nil
	}
	rawStep := span / float64(maxTicks)
	mag := math.Pow(10, math.Floor(math.Log10(rawStep)))
	residual := rawStep / mag
	var step float64
	switch {
	case residual <= 1.5:
		step = mag
	case residual <= 3.5:
		step = 2 * mag
	case residual <= 7.5:
		step = 5 * mag
	default:
		step = 10 * mag
	}
	start := math.Ceil(vmin/step) * step
	out := []float64{}
	v := start
	for v <= vmax+step*0.5 {
		out = append(out, v)
		v += step
		if len(out) > maxTicks*2 {
			break
		}
	}
	return out
}

// formatTickLabel formats a numeric tick with enough precision to be
// distinguishable but not noisy.
func formatTickLabel(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs == 0:
		return "0"
	case abs >= 1000:
		return fmt.Sprintf("%.0f", v)
	case abs >= 100:
		return fmt.Sprintf("%.1f", v)
	case abs >= 1:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%.4f", v)
	}
}

// niceTimeTicks picks a "nice" time step (1m, 2m, 5m, 10m, 15m, 30m, 1h,
// 2h, 4h, 12h, 1d) such that the chart shows a manageable number of
// labels. Inputs and outputs are Unix-epoch seconds.
func niceTimeTicks(tmin, tmax float64, maxTicks int) []float64 {
	if maxTicks < 2 {
		return nil
	}
	span := tmax - tmin
	if !(span > 0) || math.IsInf(span, 0) || math.IsNaN(span) {
		return nil
	}
	steps := []float64{
		60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 43200, 86400,
	}
	target := maxTicks / 10
	if target < 3 {
		target = 3
	}
	step := steps[len(steps)-1]
	for _, s := range steps {
		if int(span/s) <= target {
			step = s
			break
		}
	}
	start := math.Ceil(tmin/step) * step
	out := []float64{}
	v := start
	for v <= tmax {
		out = append(out, v)
		v += step
		if len(out) > maxTicks*2 {
			break
		}
	}
	return out
}

// formatTimeTickLabel converts a Unix-epoch seconds value into HH:MM
// (or HH:MM:SS when the value isn't on a minute boundary). Uses local
// time so the chart axis matches the operator's terminal clock.
func formatTimeTickLabel(epochSecs float64) string {
	t := time.Unix(int64(epochSecs), 0)
	if t.Second() == 0 {
		return t.Format("15:04")
	}
	return t.Format("15:04:05")
}

// drawYAxis paints the vertical axis line, tick marks, and right-aligned
// numeric labels into the given Y-axis rect. yMin/yMax frame the data
// space; the plot rectangle (where data is drawn, immediately to the
// right of the Y-axis) is what the tick row index needs to align with.
func drawYAxis(g *cellGrid, area rect, plot rect, yMin, yMax float64) {
	axisCol := area.right() - 1
	axisStyle := style{useFg: true, fg: 8, dim: true}
	labelStyle := style{useFg: true, fg: 7}

	for r := area.y; r < area.bottom(); r++ {
		g.set(axisCol, r, axisVertical, axisStyle)
	}

	ticks := niceTicks(yMin, yMax, plot.h)
	for _, v := range ticks {
		row := mapY(v, yMin, yMax, plot.h) + plot.y
		if row < area.y || row >= area.bottom() {
			continue
		}
		g.set(axisCol, row, axisTickLeft, axisStyle)
		label := formatTickLabel(v)
		// Right-align inside [area.x, axisCol-1].
		end := axisCol - 1
		start := end - len(label) + 1
		if start < area.x {
			start = area.x
		}
		g.drawText(start, row, label, labelStyle)
	}
}

// drawXAxis paints the horizontal axis line at the top of the X-axis
// rect and writes tick labels one row below it. xMin/xMax describe the
// data range mapped across the plot rect.
func drawXAxis(g *cellGrid, area rect, plot rect, xMin, xMax float64, timeAxis bool) {
	axisRow := area.y
	axisStyle := style{useFg: true, fg: 8, dim: true}
	labelStyle := style{useFg: true, fg: 7}

	for c := area.x; c < area.right(); c++ {
		g.set(c, axisRow, axisHorizontal, axisStyle)
	}
	if area.x > 0 {
		g.set(area.x-1, axisRow, axisCornerBL, axisStyle)
	}

	// Cap tick count by likely label width so numeric axis labels
	// don't pile on top of each other. Each numeric label is ~5 chars
	// plus a 2-char gap; time labels need ~10 chars each.
	var ticks []float64
	var minGap int
	if timeAxis {
		ticks = niceTimeTicks(xMin, xMax, area.w)
		minGap = 10
	} else {
		maxTicks := area.w / 7
		if maxTicks < 2 {
			maxTicks = 2
		}
		ticks = niceTicks(xMin, xMax, maxTicks)
		minGap = 2
	}
	lastEnd := -1

	for _, v := range ticks {
		col := mapX(v, xMin, xMax, plot.w) + plot.x
		if col < area.x || col >= area.right() {
			continue
		}
		var label string
		if timeAxis {
			label = formatTimeTickLabel(v)
		} else {
			label = formatTickLabel(v)
		}
		half := len(label) / 2
		start := col - half
		if start < area.x {
			start = area.x
		}
		if lastEnd >= 0 && start < lastEnd+minGap {
			continue
		}
		// Tick mark only painted when its label survived the spacing
		// filter — otherwise the bottom rail looks like ┬┬┬┬┬┬ with no
		// labels under it, which is noisier than a plain ─ rail.
		g.set(col, axisRow, axisTickDown, axisStyle)
		if axisRow+1 < g.h {
			g.drawText(start, axisRow+1, label, labelStyle)
			lastEnd = start + len(label)
		}
	}
}

// drawLegend writes one "● label" swatch per series left-to-right,
// wrapping to the next legend row when a series's colors dot doesn't
// fit — degrading gracefully for a long tag list rather than
// truncating the list silently. Colors come from seriesColors, indexed
// by the series' position (same index renderMultiLine used to draw it),
// so a swatch always matches its line.
func drawLegend(g *cellGrid, area rect, series []NamedSeries, colors []uint8) {
	if area.w <= 0 || area.h <= 0 {
		return
	}
	labelStyle := style{useFg: true, fg: 7}
	col, row := area.x, area.y
	for i, s := range series {
		label := truncateWidth(s.Label, area.w)
		entryW := 2 + len([]rune(label)) // "● " + label
		if col > area.x && col+entryW > area.right() {
			row++
			col = area.x
			if row >= area.bottom() {
				return
			}
		}
		fg := colors[i%len(colors)]
		g.set(col, row, '●', style{useFg: true, fg: fg, bold: true})
		g.drawText(col+2, row, label, labelStyle)
		col += entryW + 2 // gap before the next entry
	}
}

// mapX maps a data x value to a plot column in [0, plotW). Returns the
// rounded-and-clamped integer column.
func mapX(x, xmin, xmax float64, plotW int) int {
	if plotW <= 0 {
		return 0
	}
	span := xmax - xmin
	if span <= 0 {
		return 0
	}
	col := int(math.Round((x - xmin) / span * float64(plotW)))
	if col < 0 {
		col = 0
	}
	if col >= plotW {
		col = plotW - 1
	}
	return col
}

// mapY maps a data y value to a plot row in [0, plotH). Y is inverted:
// max-data → row 0, min-data → row plotH-1.
func mapY(y, ymin, ymax float64, plotH int) int {
	if plotH <= 0 {
		return 0
	}
	span := ymax - ymin
	if span <= 0 {
		return 0
	}
	row := int(math.Round((1 - (y-ymin)/span) * float64(plotH)))
	if row < 0 {
		row = 0
	}
	if row >= plotH {
		row = plotH - 1
	}
	return row
}
