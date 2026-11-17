package termchart

const fullBlock = '█'

// BarSlots is how many bars fit side by side in a Bar(w, h) chart with
// its axis drawn: one per plot column. Callers holding raw data can
// bin to this many groups themselves; Render averages anything past it.
func BarSlots(w int) int {
	return max(w-yAxisWidth, 0)
}

// barGeometry is the bar width and left-edge stride for n bars across a
// plot plotW columns wide: a bar plus a half-bar gap each while that
// fits, then 1-column bars with a 1-column gap, then no gap at all.
func barGeometry(n, plotW int) (barW, step int) {
	barW = plotW / (n + (n+1)/2)
	switch {
	case barW >= 1:
		return barW, barW + max(barW/2, 1)
	case 2*n <= plotW:
		return 1, 2
	default:
		return 1, 1
	}
}

// binBars averages consecutive bars into at most slots bins, each
// labelled with its first member's label, so a chart with more values
// than columns shows all of them instead of clipping the tail.
func binBars(labels []string, values []float64, slots int) ([]string, []float64) {
	if slots <= 0 || len(values) <= slots {
		return labels, values
	}
	per := (len(values) + slots - 1) / slots
	var outL []string
	var outV []float64
	for i := 0; i < len(values); i += per {
		end := min(i+per, len(values))
		sum := 0.0
		for _, v := range values[i:end] {
			sum += v
		}
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		outL = append(outL, label)
		outV = append(outV, sum/float64(end-i))
	}
	return outL, outV
}

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
	barW, step := barGeometry(len(values), plot.w)
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
