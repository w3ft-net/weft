package chart

// braille rasterises lines/curves at 2× horizontal and 4× vertical
// resolution by packing each character cell as a 2×4 dot pattern from
// the Unicode braille block (U+2800..U+28FF).
//
// Bit layout inside one cell (column, row → bit index):
//
//	(0,0)→0  (1,0)→3
//	(0,1)→1  (1,1)→4
//	(0,2)→2  (1,2)→5
//	(0,3)→6  (1,3)→7

const brailleOffset = 0x2800

var brailleDotBits = [2][4]uint{
	{0, 1, 2, 6}, // column 0
	{3, 4, 5, 7}, // column 1
}

type brailleCanvas struct {
	w, h int
	dots [][]bool // dots[y][x], dimensions = (h*4, w*2)
}

func newBrailleCanvas(w, h int) *brailleCanvas {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	dy := h * 4
	dx := w * 2
	dots := make([][]bool, dy)
	for i := range dots {
		dots[i] = make([]bool, dx)
	}
	return &brailleCanvas{w: w, h: h, dots: dots}
}

func (c *brailleCanvas) dotW() int { return c.w * 2 }
func (c *brailleCanvas) dotH() int { return c.h * 4 }

func (c *brailleCanvas) setDot(x, y int) {
	if x < 0 || y < 0 || x >= c.dotW() || y >= c.dotH() {
		return
	}
	c.dots[y][x] = true
}

// drawLine plots a Bresenham line in dot space.
func (c *brailleCanvas) drawLine(x0, y0, x1, y1 int) {
	x, y := x0, y0
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx := 1
	if x0 >= x1 {
		sx = -1
	}
	sy := 1
	if y0 >= y1 {
		sy = -1
	}
	err := dx + dy
	for {
		c.setDot(x, y)
		if x == x1 && y == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x += sx
		}
		if e2 <= dx {
			err += dx
			y += sy
		}
	}
}

// drawSmoothLine draws a Catmull-Rom spline through the given dot-space
// points. Endpoints are duplicated as control points so the curve still
// passes through the first and last points.
func (c *brailleCanvas) drawSmoothLine(points [][2]float64) {
	n := len(points)
	if n < 2 {
		return
	}
	if n == 2 {
		c.drawLine(int(points[0][0]), int(points[0][1]), int(points[1][0]), int(points[1][1]))
		return
	}
	for i := 0; i < n-1; i++ {
		var p0, p3 [2]float64
		p1 := points[i]
		p2 := points[i+1]
		if i == 0 {
			p0 = points[0]
		} else {
			p0 = points[i-1]
		}
		if i+2 < n {
			p3 = points[i+2]
		} else {
			p3 = points[n-1]
		}
		dx := p2[0] - p1[0]
		dy := p2[1] - p1[1]
		dist := dx*dx + dy*dy
		// integer steps proportional to segment length, min 4
		steps := 4
		if d := int(sqrt(dist)); d > steps {
			steps = d
		}
		prevX := int(p1[0])
		prevY := int(p1[1])
		for step := 1; step <= steps; step++ {
			t := float64(step) / float64(steps)
			t2 := t * t
			t3 := t2 * t
			x := 0.5 * ((2.0 * p1[0]) +
				(-p0[0]+p2[0])*t +
				(2.0*p0[0]-5.0*p1[0]+4.0*p2[0]-p3[0])*t2 +
				(-p0[0]+3.0*p1[0]-3.0*p2[0]+p3[0])*t3)
			y := 0.5 * ((2.0 * p1[1]) +
				(-p0[1]+p2[1])*t +
				(2.0*p0[1]-5.0*p1[1]+4.0*p2[1]-p3[1])*t2 +
				(-p0[1]+3.0*p1[1]-3.0*p2[1]+p3[1])*t3)
			cx := int(x)
			cy := int(y)
			c.drawLine(prevX, prevY, cx, cy)
			prevX = cx
			prevY = cy
		}
	}
}

// renderToGrid stamps the dot grid into cells starting at (offX, offY)
// using the given style. Empty cells are left untouched so background
// content (axis lines, gridlines) shows through.
func (c *brailleCanvas) renderToGrid(g *cellGrid, offX, offY int, st style) {
	for cy := 0; cy < c.h; cy++ {
		for cx := 0; cx < c.w; cx++ {
			var bits uint
			for dx := 0; dx < 2; dx++ {
				for dy := 0; dy < 4; dy++ {
					y := cy*4 + dy
					x := cx*2 + dx
					if y < c.dotH() && x < c.dotW() && c.dots[y][x] {
						bits |= 1 << brailleDotBits[dx][dy]
					}
				}
			}
			if bits != 0 {
				g.set(cx+offX, cy+offY, rune(brailleOffset+int(bits)), st)
			}
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sqrt is a tiny Newton's-method square root so this file stays
// dependency-free of the rest of math (one less import to bloat the wasm
// binary). Accuracy is fine for "how many spline interpolation steps
// should I take".
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 8; i++ {
		z = (z + x/z) * 0.5
	}
	return z
}
