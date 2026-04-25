package chart

import (
	"strings"
)

// Point is one (x, y) data point for line / bar input. X is typically a
// Unix-epoch second (when XAxisTime is set) or a numeric index;
// Y is the value to plot.
type Point struct {
	X, Y float64
}

// Severity tags an alert threshold. Drives the color of the threshold
// line on a chart.
type Severity int

const (
	SeverityWarn Severity = iota
	SeverityCrit
	SeverityInfo
)

// XAxisFormat selects how the X-axis tick values are rendered.
type XAxisFormat int

const (
	XAxisNumeric XAxisFormat = iota // 0, 5, 10, 15, …
	XAxisTime                       // 14:23, 14:24, …
)

// Builder is the fluent surface for one chart render. The shellcore
// `chart` builtin and `ls -l /cron.d/` row glyph both go through
// Sparkline / Builder.
type Builder struct {
	w, h    int
	title   string
	points  []Point
	xAxis   XAxisFormat
	thresh  *threshold
	noAxis  bool
	chart   chartKind
	bars    []float64 // bar mode: per-tag heights, parallel to barLabels
	barLabels []string
}

type threshold struct {
	value    float64
	severity Severity
}

type chartKind int

const (
	kindLine chartKind = iota
	kindBar
)

// Line creates a line-chart builder sized to a (cols, rows) terminal.
// The total render footprint is (w, h); axes and title eat into that.
func Line(w, h int) *Builder {
	return &Builder{w: w, h: h, chart: kindLine}
}

// Bar creates a vertical-bar chart builder. Use BarValues to set the
// data; X-axis is categorical and labels go under each bar.
func Bar(w, h int) *Builder {
	return &Builder{w: w, h: h, chart: kindBar}
}

func (b *Builder) Title(s string) *Builder {
	b.title = s
	return b
}

func (b *Builder) Points(pts []Point) *Builder {
	b.points = pts
	return b
}

func (b *Builder) XAxisTime() *Builder {
	b.xAxis = XAxisTime
	return b
}

func (b *Builder) YThreshold(value float64, sev Severity) *Builder {
	b.thresh = &threshold{value: value, severity: sev}
	return b
}

// NoAxis suppresses the y-axis labels and the x-axis ruler. Useful for
// inline mini-charts where the surrounding text already explains the
// scale.
func (b *Builder) NoAxis() *Builder {
	b.noAxis = true
	return b
}

// BarValues sets the per-bar (label, height) data for a bar chart. The
// X-axis is categorical; labels appear under each bar.
func (b *Builder) BarValues(labels []string, values []float64) *Builder {
	b.barLabels = labels
	b.bars = values
	return b
}

// Render produces the chart as an ANSI string with embedded newlines.
// Output is empty for non-positive dimensions or empty data.
func (b *Builder) Render() (string, error) {
	if b.w <= 0 || b.h <= 0 {
		return "", nil
	}
	g := newCellGrid(b.w, b.h)

	hasTitle := b.title != ""
	withAxis := !b.noAxis
	l := computeLayout(b.w, b.h, hasTitle, withAxis)

	if hasTitle && l.hasTitle {
		drawTitle(g, l.title, b.title)
	}

	switch b.chart {
	case kindLine:
		if len(b.points) >= 2 && l.plot.w > 0 && l.plot.h > 0 {
			xMin, xMax, yMin, yMax := dataExtent(b.points)
			if withAxis {
				drawYAxis(g, l.yAxis, l.plot, yMin, yMax)
				drawXAxis(g, l.xAxis, l.plot, xMin, xMax, b.xAxis == XAxisTime)
			}
			renderLine(g, l.plot, b.points, xMin, xMax, yMin, yMax)
			if b.thresh != nil {
				renderThreshold(g, l.plot, b.thresh.value, yMin, yMax, b.thresh.severity)
			}
		} else if len(b.points) == 1 && l.plot.w > 0 && l.plot.h > 0 {
			// Single point: show the value centered as a numeric readout.
			drawSingleValue(g, l.plot, b.points[0].Y)
		}
	case kindBar:
		if len(b.bars) > 0 && l.plot.w > 0 && l.plot.h > 0 {
			yMin, yMax := minMax(b.bars)
			if yMin > 0 {
				yMin = 0
			}
			if yMax < 0 {
				yMax = 0
			}
			if withAxis {
				drawYAxis(g, l.yAxis, l.plot, yMin, yMax)
				drawBarXLabels(g, l.xAxis, l.plot, b.barLabels)
			}
			renderBars(g, l.plot, b.bars, yMin, yMax)
		}
	}

	return g.renderANSI(), nil
}

// drawTitle places the title centered (with ellipsis if too wide) at
// the top of the chart in bold orange — same accent color as the
// shell's running spinner.
func drawTitle(g *cellGrid, area rect, title string) {
	st := style{useFg: true, fg: 208, bold: true}
	t := truncateWidth(title, area.w)
	col := area.x + (area.w-len(t))/2
	if col < area.x {
		col = area.x
	}
	g.drawText(col, area.y, t, st)
}

// drawSingleValue prints "value" centered in the plot area when only
// one data point is available — a line chart needs at least two.
func drawSingleValue(g *cellGrid, plot rect, v float64) {
	st := style{useFg: true, fg: 6, bold: true}
	label := formatTickLabel(v)
	col := plot.x + (plot.w-len(label))/2
	row := plot.y + plot.h/2
	g.drawText(col, row, label, st)
}

// drawBarXLabels writes one centered label under each bar, skipping
// labels that would overlap with their neighbour.
func drawBarXLabels(g *cellGrid, area rect, plot rect, labels []string) {
	if len(labels) == 0 {
		return
	}
	st := style{useFg: true, fg: 7}
	axisRow := area.y
	axisStyle := style{useFg: true, fg: 8, dim: true}
	for c := area.x; c < area.right(); c++ {
		g.set(c, axisRow, axisHorizontal, axisStyle)
	}
	if area.x > 0 {
		g.set(area.x-1, axisRow, axisCornerBL, axisStyle)
	}
	n := len(labels)
	barW := plot.w / (n + (n+1)/2)
	if barW < 1 {
		barW = 1
	}
	gap := barW / 2
	if gap < 1 {
		gap = 1
	}
	step := barW + gap
	if axisRow+1 >= g.h {
		return
	}
	lastEnd := -1
	for i, label := range labels {
		barX := i * step
		if barX >= plot.w {
			break
		}
		center := plot.x + barX + barW/2
		t := truncateWidth(label, step)
		start := center - len(t)/2
		if start < area.x {
			start = area.x
		}
		if lastEnd >= 0 && start <= lastEnd {
			continue
		}
		g.drawText(start, axisRow+1, t, st)
		lastEnd = start + len(t)
	}
}

func minMax(values []float64) (float64, float64) {
	vmin, vmax := values[0], values[0]
	for _, v := range values[1:] {
		if v < vmin {
			vmin = v
		}
		if v > vmax {
			vmax = v
		}
	}
	return vmin, vmax
}

// truncateWidth returns at most maxW characters, with an ellipsis when
// truncation occurred.
func truncateWidth(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if len([]rune(s)) <= maxW {
		return s
	}
	if maxW == 1 {
		return "…"
	}
	rs := []rune(s)
	return string(rs[:maxW-1]) + "…"
}

// trimTrailingBlanks strips trailing all-blank rows from a rendered
// chart. Useful when a chart's reserved height is taller than what was
// drawn (e.g. a 2-bar bar-chart in a 20-row footprint shouldn't print
// 18 rows of empty space).
func trimTrailingBlanks(s string) string {
	lines := strings.Split(s, "\n")
	end := len(lines)
	for end > 0 {
		stripped := stripANSILocal(lines[end-1])
		if strings.TrimRight(stripped, " ") != "" {
			break
		}
		end--
	}
	return strings.Join(lines[:end], "\n")
}

// stripANSILocal removes SGR escape sequences. (Local copy so the chart
// package stays freestanding from the rest of shellcore.)
func stripANSILocal(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
