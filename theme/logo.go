package theme

import (
	"fmt"
	"strings"
)

// Warp thread colors (cool blues/indigos) — one per column of the loom.
var warpColors = []int{33, 69, 39, 63, 75, 27, 68, 105, 32, 74}

// Weft thread colors (warm oranges/golds) — one per row of the shuttle pass.
var weftColors = []int{208, 173, 214, 209, 215}

// Letter bitmaps — 2-cell-wide strokes so the interlacing ║═ pattern
// is visible within every bar. '#' = filled, ' ' = empty.
var letterW = []string{
	"##    ##    ##",
	"##    ##    ##",
	" ##  ####  ## ",
	"  ####  ####  ",
	"   ##    ##   ",
}

var letterE = []string{
	"##########",
	"        ##",
	"  ########",
	"        ##",
	"##########",
}

var letterF = []string{
	"##########",
	"##        ",
	"########  ",
	"##        ",
	"##        ",
}

var letterT = []string{
	"##########",
	"    ##    ",
	"    ##    ",
	"    ##    ",
	"    ##    ",
}

// RenderLogo returns the colored "WEFT" wordmark shown at startup. Each
// filled cell renders
// as ║ (warp on top) or ═ (weft on top) in an alternating interlace
// pattern — like threads crossing in fabric.
func RenderLogo() string {
	gap := "   "
	rows := len(letterW)
	var lines []string
	for r := 0; r < rows; r++ {
		lines = append(lines, letterW[r]+gap+letterE[r]+gap+letterF[r]+gap+letterT[r])
	}

	var b strings.Builder
	for r, line := range lines {
		for c, ch := range line {
			if ch == ' ' {
				b.WriteByte(' ')
				continue
			}
			if (r+c)%2 == 0 {
				fmt.Fprintf(&b, "\033[38;5;%dm║\033[0m", warpColors[c%len(warpColors)])
			} else {
				fmt.Fprintf(&b, "\033[38;5;%dm═\033[0m", weftColors[r%len(weftColors)])
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
