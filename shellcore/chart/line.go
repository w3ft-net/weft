package chart

import "math"

// renderLine draws a Catmull-Rom-interpolated line of the given points
// into the plot area of g, using a braille canvas for 2× horizontal /
// 4× vertical resolution. xMin/xMax/yMin/yMax frame the data space; the
// plot rect describes where in g the data should land.
//
// Returns silently for fewer than 2 points.
func renderLine(g *cellGrid, plot rect, pts []Point, xMin, xMax, yMin, yMax float64) {
	if len(pts) < 2 || plot.w <= 0 || plot.h <= 0 {
		return
	}
	canvas := newBrailleCanvas(plot.w, plot.h)

	// Map each data point to dot-space.
	dotPts := make([][2]float64, len(pts))
	for i, p := range pts {
		dotX := mapDotX(p.X, xMin, xMax, plot.w)
		dotY := mapDotY(p.Y, yMin, yMax, plot.h)
		dotPts[i] = [2]float64{dotX, dotY}
	}
	canvas.drawSmoothLine(dotPts)

	canvas.renderToGrid(g, plot.x, plot.y, style{useFg: true, fg: 6, bold: true})
}

// renderThreshold draws a horizontal dashed line at y=value across the
// plot area. Used to show alert thresholds on a watch chart. severity
// maps to color: warn (yellow=3), crit (red=1), info (cyan=6).
func renderThreshold(g *cellGrid, plot rect, value, yMin, yMax float64, severity Severity) {
	if plot.w <= 0 || plot.h <= 0 {
		return
	}
	if value < yMin || value > yMax {
		return
	}
	row := mapY(value, yMin, yMax, plot.h) + plot.y
	if row < plot.y || row >= plot.bottom() {
		return
	}
	var fg uint8 = 3
	switch severity {
	case SeverityCrit:
		fg = 1
	case SeverityInfo:
		fg = 6
	}
	st := style{useFg: true, fg: fg, dim: true}
	for c := plot.x; c < plot.right(); c++ {
		// Dashed: write '╌' every other cell.
		if (c-plot.x)%2 == 0 {
			g.set(c, row, '╌', st)
		}
	}
}

// mapDotX maps a data x to a dot-space column in [0, plotW*2).
func mapDotX(x, xmin, xmax float64, plotW int) float64 {
	span := xmax - xmin
	if span <= 0 {
		return 0
	}
	v := (x - xmin) / span * float64(plotW*2)
	if v < 0 {
		v = 0
	}
	if v >= float64(plotW*2) {
		v = float64(plotW*2 - 1)
	}
	return v
}

// mapDotY maps a data y to a dot-space row in [0, plotH*4); inverted.
func mapDotY(y, ymin, ymax float64, plotH int) float64 {
	span := ymax - ymin
	if span <= 0 {
		return 0
	}
	v := (1.0 - (y-ymin)/span) * float64(plotH*4)
	if v < 0 {
		v = 0
	}
	if v >= float64(plotH*4) {
		v = float64(plotH*4 - 1)
	}
	return v
}

// dataExtent returns (xMin, xMax, yMin, yMax) for the given points,
// padding the y-range by ~5 % so the line doesn't ride the top or
// bottom edge of the plot. Empty input returns zeroes.
func dataExtent(pts []Point) (xMin, xMax, yMin, yMax float64) {
	if len(pts) == 0 {
		return 0, 0, 0, 0
	}
	xMin, xMax = pts[0].X, pts[0].X
	yMin, yMax = pts[0].Y, pts[0].Y
	for _, p := range pts[1:] {
		if p.X < xMin {
			xMin = p.X
		}
		if p.X > xMax {
			xMax = p.X
		}
		if p.Y < yMin {
			yMin = p.Y
		}
		if p.Y > yMax {
			yMax = p.Y
		}
	}
	if yMin == yMax {
		// Single y-value: artificially widen so the line lands mid-frame.
		if yMin == 0 {
			yMin, yMax = -1, 1
		} else {
			pad := math.Abs(yMin) * 0.5
			yMin -= pad
			yMax += pad
		}
	} else {
		pad := (yMax - yMin) * 0.05
		yMin -= pad
		yMax += pad
	}
	return
}
