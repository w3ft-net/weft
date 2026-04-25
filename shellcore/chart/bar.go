package chart

const fullBlock = '█'

// renderBars draws a vertical bar chart into the plot area. Each
// (label, value) pair becomes a single bar; bars share equal width and
// gap. yMin/yMax frame the data range. Negative values are colored
// "bear" red (1); non-negative are colored "bull" green (2).
//
// Bar labels are not drawn here — the caller is responsible for
// labeling (e.g. via a legend row beneath the plot, since the X-axis
// for a bar chart is categorical, not numeric).
func renderBars(g *cellGrid, plot rect, values []float64, yMin, yMax float64) {
	if len(values) == 0 || plot.w <= 0 || plot.h <= 0 {
		return
	}
	n := len(values)
	// 1 bar + half-bar gap per bar, width-equal. Minimum 1 col bar.
	barW := plot.w / (n + (n+1)/2)
	if barW < 1 {
		barW = 1
	}
	gap := barW / 2
	if gap < 1 {
		gap = 1
	}
	step := barW + gap
	span := yMax - yMin
	for i, v := range values {
		barX := i * step
		if barX >= plot.w {
			break
		}
		var frac float64
		if span > 0 {
			frac = (v - yMin) / span
		} else {
			frac = 0.5
		}
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		barRows := int(frac*float64(plot.h) + 0.5)
		if barRows == 0 && v != yMin {
			barRows = 1
		}
		var fg uint8 = 2
		if v < 0 {
			fg = 1
		}
		st := style{useFg: true, fg: fg}
		for r := 0; r < barRows; r++ {
			row := plot.y + plot.h - 1 - r
			for c := 0; c < barW && barX+c < plot.w; c++ {
				g.set(plot.x+barX+c, row, fullBlock, st)
			}
		}
	}
}
