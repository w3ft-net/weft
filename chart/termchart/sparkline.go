package termchart

import "strings"

// blockRamp is the 1-cell-tall Unicode sparkline ramp from "▁" up to
// "█" (U+2581 .. U+2588). 8 levels.
var blockRamp = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline returns a one-row string of Unicode block characters
// representing the values normalised between min(values) and max(values).
// An empty slice returns "". A single value (or all-equal values) maps
// to the lowest block.
func Sparkline(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	vmin, vmax := values[0], values[0]
	for _, v := range values[1:] {
		if v < vmin {
			vmin = v
		}
		if v > vmax {
			vmax = v
		}
	}
	span := vmax - vmin
	var b strings.Builder
	b.Grow(len(values) * 4)
	for _, v := range values {
		idx := 0
		if span > 0 {
			n := (v - vmin) / span
			s := int(n*7 + 0.5)
			if s < 0 {
				s = 0
			}
			if s > 7 {
				s = 7
			}
			idx = s
		}
		b.WriteRune(blockRamp[idx])
	}
	return b.String()
}

// SparklineColored is Sparkline with each block colored: values above
// the mean render in green, values at-or-below render in red. Useful as
// the per-watch glyph in `ls -l /cron.d/`.
func SparklineColored(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	vmin, vmax := values[0], values[0]
	sum := 0.0
	for _, v := range values {
		if v < vmin {
			vmin = v
		}
		if v > vmax {
			vmax = v
		}
		sum += v
	}
	mean := sum / float64(len(values))
	span := vmax - vmin

	var b strings.Builder
	b.Grow(len(values) * 16)
	for _, v := range values {
		idx := 0
		if span > 0 {
			n := (v - vmin) / span
			s := int(n*7 + 0.5)
			if s < 0 {
				s = 0
			}
			if s > 7 {
				s = 7
			}
			idx = s
		}
		if v > mean {
			b.WriteString("\x1b[38;5;2m")
		} else {
			b.WriteString("\x1b[38;5;1m")
		}
		b.WriteRune(blockRamp[idx])
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
