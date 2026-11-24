// Package termchart renders line / sparkline / bar charts as ANSI strings
// for embedding shells. It is renderer-agnostic: the only output is a
// string with embedded newlines and SGR escape sequences, suitable for
// piping through the shellcore write callback used by both the CLI
// and the wasm-backed browser shell.
//
// The render algorithms (cell grid, half-block / braille rasterisation,
// nice-step axis tick selection) are translated from termichart-render
// and termichart-charts (https://github.com/HeyElsa/terminal-chart),
// minus the candlestick / theme / interactive / wasm / language-binding
// surface we don't need.
package termchart

import "strings"

// style describes the SGR attributes for one cell. Colors are ANSI
// 256-palette indices to match the rest of shellcore (see shellcore/ansi.go);
// a value of 0 with usefg=false means "default foreground/background".
type style struct {
	fg    uint8
	bg    uint8
	useFg bool
	useBg bool
	bold  bool
	dim   bool
}

func (s style) equal(o style) bool {
	return s.fg == o.fg && s.bg == o.bg &&
		s.useFg == o.useFg && s.useBg == o.useBg &&
		s.bold == o.bold && s.dim == o.dim
}

// cellGrid is a row-major grid of styled rune cells. Out-of-bounds
// writes are silently dropped — chart code happily computes coordinates
// that fall outside the plot area.
type cellGrid struct {
	w, h   int
	runes  []rune
	styles []style
}

func newCellGrid(w, h int) *cellGrid {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	n := w * h
	g := &cellGrid{w: w, h: h, runes: make([]rune, n), styles: make([]style, n)}
	for i := range g.runes {
		g.runes[i] = ' '
	}
	return g
}

func (g *cellGrid) inBounds(col, row int) bool {
	return col >= 0 && row >= 0 && col < g.w && row < g.h
}

func (g *cellGrid) set(col, row int, ch rune, st style) {
	if !g.inBounds(col, row) {
		return
	}
	idx := row*g.w + col
	g.runes[idx] = ch
	g.styles[idx] = st
}

// drawText writes text horizontally starting at (col, row); characters
// past the right edge are dropped.
func (g *cellGrid) drawText(col, row int, text string, st style) {
	c := col
	for _, r := range text {
		g.set(c, row, r, st)
		c++
	}
}

// renderANSI flattens the grid into a printable string. It tracks the
// previous style and only emits SGR changes when needed, and appends
// `\x1b[0m\x1b[K\n` at the end of each row so a redraw over a previous
// frame doesn't leak ghost characters past the new content's right edge.
func (g *cellGrid) renderANSI() string {
	var b strings.Builder
	b.Grow(g.w * g.h * 4)

	var prev style
	for row := 0; row < g.h; row++ {
		if row > 0 {
			b.WriteByte('\n')
			prev = style{}
		}
		for col := 0; col < g.w; col++ {
			idx := row*g.w + col
			st := g.styles[idx]
			if !st.equal(prev) {
				emitStyleDiff(&b, prev, st)
				prev = st
			}
			b.WriteRune(g.runes[idx])
		}
		// Reset and erase to end of line so an in-place redraw doesn't
		// leak content from a wider previous frame.
		b.WriteString("\x1b[0m\x1b[K")
		prev = style{}
	}
	b.WriteByte('\n')
	return b.String()
}

// emitStyleDiff writes the minimal SGR sequence to transition from
// prev to next. ANSI has no portable "un-bold" / "un-dim" so any
// transition that turns an attribute off is implemented as a full
// reset followed by re-enabling what's still on.
func emitStyleDiff(b *strings.Builder, prev, next style) {
	if (next == style{}) {
		b.WriteString("\x1b[0m")
		return
	}
	needReset := (prev.bold && !next.bold) || (prev.dim && !next.dim)
	if needReset {
		b.WriteString("\x1b[0m")
		if next.bold {
			b.WriteString("\x1b[1m")
		}
		if next.dim {
			b.WriteString("\x1b[2m")
		}
		if next.useFg {
			emitFg(b, next.fg)
		}
		if next.useBg {
			emitBg(b, next.bg)
		}
		return
	}
	if next.bold && !prev.bold {
		b.WriteString("\x1b[1m")
	}
	if next.dim && !prev.dim {
		b.WriteString("\x1b[2m")
	}
	if next.useFg != prev.useFg || next.fg != prev.fg {
		if next.useFg {
			emitFg(b, next.fg)
		} else {
			b.WriteString("\x1b[39m")
		}
	}
	if next.useBg != prev.useBg || next.bg != prev.bg {
		if next.useBg {
			emitBg(b, next.bg)
		} else {
			b.WriteString("\x1b[49m")
		}
	}
}

func emitFg(b *strings.Builder, n uint8) {
	b.WriteString("\x1b[38;5;")
	writeUint8(b, n)
	b.WriteByte('m')
}

func emitBg(b *strings.Builder, n uint8) {
	b.WriteString("\x1b[48;5;")
	writeUint8(b, n)
	b.WriteByte('m')
}

func writeUint8(b *strings.Builder, n uint8) {
	if n >= 100 {
		b.WriteByte('0' + n/100)
	}
	if n >= 10 {
		b.WriteByte('0' + (n/10)%10)
	}
	b.WriteByte('0' + n%10)
}
