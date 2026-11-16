package ingest

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// inferLayouts deduces Go time layouts from sample timestamp texts, using
// nothing but the calendar and the clock: no table of known formats. Each text
// is split into digit runs, letter runs and single punctuation marks; the
// texts must share one shape; calendar and clock roles are assigned by
// position and by what the numbers can be (a clock reads hour, minute,
// second; a component above 12 cannot be a month); and a layout is kept only
// if time.Parse accepts every text with it and it names a full date. When
// day/month order is ambiguous both layouts are returned, the one that puts
// the texts in chronological order first.
func inferLayouts(texts []string) []string {
	if len(texts) == 0 {
		return nil
	}
	toks := make([][]token, len(texts))
	for i, t := range texts {
		toks[i] = tokenize(t)
	}
	shapes, ok := commonShape(toks)
	if !ok {
		return nil
	}
	g := &layoutGen{shapes: shapes}
	for _, sh := range shapes {
		if sh.kind == tokLetters && allIn(sh.vals, "AM", "PM", "am", "pm") {
			g.hasAMPM = true
		}
	}
	g.walk(0, nil, 0)

	var layouts []string
	seen := map[string]bool{}
	for _, l := range g.out {
		if seen[l] {
			continue
		}
		seen[l] = true
		if parsesAll(l, texts) {
			layouts = append(layouts, l)
		}
	}
	return rankChronologically(layouts, texts)
}

type tokKind int

const (
	tokDigits tokKind = iota
	tokLetters
	tokOther
)

type token struct {
	kind tokKind
	text string
}

func tokenize(s string) []token {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		j := i
		switch {
		case c >= '0' && c <= '9':
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, token{tokDigits, s[i:j]})
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			out = append(out, token{tokLetters, s[i:j]})
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			j = i + size
			out = append(out, token{tokOther, s[i:j]})
		}
		i = j
	}
	return out
}

// shape is one token position across all the texts.
type shape struct {
	kind       tokKind
	lit        string   // punctuation: the mark (equal in every text)
	minW, maxW int      // digits: the widths seen
	vals       []string // the token's text in each text
}

func commonShape(toks [][]token) ([]shape, bool) {
	n := len(toks[0])
	if n == 0 {
		return nil, false
	}
	shapes := make([]shape, n)
	for j := 0; j < n; j++ {
		sh := shape{kind: toks[0][j].kind, minW: 1 << 30}
		for _, row := range toks {
			if len(row) != n || row[j].kind != sh.kind {
				return nil, false
			}
			sh.vals = append(sh.vals, row[j].text)
			if sh.kind == tokDigits {
				sh.minW, sh.maxW = min(sh.minW, len(row[j].text)), max(sh.maxW, len(row[j].text))
			}
			if sh.kind == tokOther {
				if row[j].text != toks[0][j].text {
					return nil, false
				}
				sh.lit = row[j].text
			}
		}
		if sh.kind == tokDigits && sh.maxW-sh.minW > 1 {
			return nil, false
		}
		shapes[j] = sh
	}
	return shapes, true
}

func allIn(vals []string, ok ...string) bool {
	for _, v := range vals {
		found := false
		for _, o := range ok {
			found = found || v == o
		}
		if !found {
			return false
		}
	}
	return true
}

func parsesAll(layout string, texts []string) bool {
	for _, t := range texts {
		if _, err := time.Parse(layout, t); err != nil {
			return false
		}
	}
	return true
}

// layoutGen enumerates layouts for a common shape by choosing, left to right,
// a role for each token (or run of tokens). Choices are cheap and few; the
// final time.Parse check is what decides.
type layoutGen struct {
	shapes  []shape
	hasAMPM bool
	out     []string
}

// Role flags: which parts of a date the layout names so far.
const (
	haveYear = 1 << iota
	haveMonth
	haveDay
	haveSeconds
	haveClock
)

func (g *layoutGen) walk(j int, pieces []string, have int) {
	if j == len(g.shapes) {
		if have&(haveYear|haveMonth|haveDay) == haveYear|haveMonth|haveDay {
			g.out = append(g.out, strings.Join(pieces, ""))
		}
		return
	}
	sh := g.shapes[j]
	switch sh.kind {
	case tokDigits:
		g.walkDigits(j, pieces, have)
	case tokLetters:
		g.walkLetters(j, pieces, have)
	default:
		g.walkOther(j, pieces, have)
	}
}

// padded picks the padded or unpadded Go element for a one-or-two digit field.
func padded(sh shape, padded, unpadded string) string {
	if sh.minW == 1 {
		return unpadded
	}
	return padded
}

func (g *layoutGen) walkDigits(j int, pieces []string, have int) {
	// A run is digit tokens joined by one repeated single-mark separator.
	run := []int{j}
	sep := ""
	for k := j; k+2 < len(g.shapes); k += 2 {
		m, nx := g.shapes[k+1], g.shapes[k+2]
		if m.kind != tokOther || nx.kind != tokDigits || (sep != "" && m.lit != sep) || !strings.Contains(":-/.", m.lit) {
			break
		}
		sep = m.lit
		run = append(run, k+2)
	}
	shapeAt := func(i int) shape { return g.shapes[run[i]] }
	emit := func(elems []string, sepLit string, flags int) {
		// A component can be named once: a second clock or a second year
		// means these digits are something else (a zone offset, say).
		if have&flags != 0 {
			return
		}
		next := append([]string(nil), pieces...)
		for i, e := range elems {
			if i > 0 {
				next = append(next, sepLit)
			}
			next = append(next, e)
		}
		g.walk(run[len(run)-1]+1, next, have|flags)
	}

	switch {
	case sep == ":" && (len(run) == 2 || len(run) == 3):
		hour := "15"
		if g.hasAMPM {
			hour = padded(shapeAt(0), "03", "3")
		}
		elems := []string{hour, padded(shapeAt(1), "04", "4")}
		flags := 0
		if len(run) == 3 {
			elems = append(elems, padded(shapeAt(2), "05", "5"))
			flags = haveSeconds
		}
		if have&haveClock != 0 {
			return
		}
		emit(elems, ":", flags|haveClock)
	case sep == ":" && len(run) == 4 && shapeAt(0).minW == 4 && shapeAt(0).maxW == 4:
		// A year then a clock, joined by ":" (e.g. 2026:12:00:06).
		if have&haveClock != 0 {
			return
		}
		emit([]string{"2006", "15", "04", "05"}, ":", haveYear|haveSeconds|haveClock)
	case (sep == "-" || sep == "/" || sep == ".") && len(run) == 3:
		a, c := shapeAt(0), shapeAt(2)
		mid := shapeAt(1)
		switch {
		case a.minW == 4 && a.maxW == 4:
			emit([]string{"2006", padded(mid, "01", "1"), padded(c, "02", "2")}, sep, haveYear|haveMonth|haveDay)
		case c.minW == 4 && c.maxW == 4:
			emit([]string{padded(a, "02", "2"), padded(mid, "01", "1"), "2006"}, sep, haveYear|haveMonth|haveDay) // day first
			emit([]string{padded(a, "01", "1"), padded(mid, "02", "2"), "2006"}, sep, haveYear|haveMonth|haveDay) // month first
		}
	case len(run) == 1:
		// A lone number: a year, or the day beside a month name.
		sh := shapeAt(0)
		switch {
		case sh.minW == 4 && sh.maxW == 4:
			if have&haveYear == 0 {
				g.walk(j+1, append(append([]string(nil), pieces...), "2006"), have|haveYear)
			}
		case sh.maxW <= 2 && g.nextToMonthName(j):
			if have&haveDay == 0 {
				g.walk(j+1, append(append([]string(nil), pieces...), padded(sh, "02", "2")), have|haveDay)
			}
		}
	}
}

// nextToMonthName reports whether the digit token at j sits right beside a
// letters token that could be a month name (so it must be the day).
func (g *layoutGen) nextToMonthName(j int) bool {
	for _, k := range []int{j - 2, j + 2} {
		if k >= 0 && k < len(g.shapes) && g.shapes[k].kind == tokLetters && allLen(g.shapes[k].vals, 3) {
			return true
		}
	}
	return false
}

func allLen(vals []string, n int) bool {
	for _, v := range vals {
		if len(v) != n {
			return false
		}
	}
	return true
}

func allUpper(vals []string) bool {
	for _, v := range vals {
		if v != strings.ToUpper(v) {
			return false
		}
	}
	return true
}

func (g *layoutGen) walkLetters(j int, pieces []string, have int) {
	sh := g.shapes[j]
	push := func(elem string, flags int) {
		g.walk(j+1, append(append([]string(nil), pieces...), elem), have|flags)
	}
	switch {
	case allIn(sh.vals, "AM", "PM"):
		push("PM", 0)
		return
	case allIn(sh.vals, "am", "pm"):
		push("pm", 0)
		return
	case allIn(sh.vals, "Z"):
		push("Z07:00", 0)
		return
	case len(sh.vals) > 0 && allUnique1(sh.vals) && sh.vals[0] == "T":
		push("T", 0)
		return
	}
	if allLen(sh.vals, 3) {
		push("Jan", haveMonth) // a month name
		push("Mon", 0)         // a weekday name
	}
	push("January", haveMonth)
	push("Monday", 0)
	// A zone abbreviation follows a clock after a space ("12:00:06 UTC");
	// elsewhere any three capital letters would pass for one.
	afterClock := have&haveClock != 0 && j >= 2 && g.shapes[j-1].lit == " " && g.shapes[j-2].kind == tokDigits
	if afterClock && allUpper(sh.vals) && allLen(sh.vals, len(sh.vals[0])) && len(sh.vals[0]) >= 3 && len(sh.vals[0]) <= 5 {
		push("MST", 0)
	}
}

func allUnique1(vals []string) bool {
	for _, v := range vals {
		if v != vals[0] {
			return false
		}
	}
	return true
}

func (g *layoutGen) walkOther(j int, pieces []string, have int) {
	sh := g.shapes[j]
	lit := func(consume int, elem string, flags int) {
		g.walk(j+consume, append(append([]string(nil), pieces...), elem), have|flags)
	}
	next := func(i int) (shape, bool) {
		if j+i < len(g.shapes) {
			return g.shapes[j+i], true
		}
		return shape{}, false
	}
	switch sh.lit {
	case "+", "-":
		if d, ok := next(1); ok && d.kind == tokDigits && d.minW == d.maxW {
			switch {
			case d.minW == 4:
				lit(2, "-0700", 0)
			case d.minW == 2:
				if c, ok := next(2); ok && c.lit == ":" {
					if m, ok := next(3); ok && m.kind == tokDigits && m.minW == 2 && m.maxW == 2 {
						lit(4, "-07:00", 0)
					}
				}
				lit(2, "-07", 0)
			}
		}
	case ".", ",":
		if d, ok := next(1); ok && d.kind == tokDigits && d.minW == d.maxW && have&haveSeconds != 0 && j > 0 && g.shapes[j-1].kind == tokDigits {
			lit(2, sh.lit+strings.Repeat("0", d.minW), 0)
			return
		}
	case "_":
		return // would read as the padded-day element
	}
	lit(1, sh.lit, 0)
}

// rankChronologically orders layouts so the one whose parse puts the texts in
// non-decreasing time order comes first; ties keep generation order.
func rankChronologically(layouts []string, texts []string) []string {
	inversions := func(layout string) int {
		n := 0
		var prev time.Time
		have := false
		for _, t := range texts {
			cur, err := time.Parse(layout, t)
			if err != nil {
				continue // a line that is not a timestamp says nothing about order
			}
			if have && cur.Before(prev) {
				n++
			}
			prev, have = cur, true
		}
		return n
	}
	score := map[string]int{}
	for _, l := range layouts {
		score[l] = inversions(l)
	}
	sort.SliceStable(layouts, func(a, b int) bool { return score[layouts[a]] < score[layouts[b]] })
	return layouts
}

// isAlnum reports whether b is an ASCII letter or digit.
func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// frameOf splits text into its leading and trailing punctuation (non-alphanumeric
// bytes) and the core between them.
func frameOf(text string) (lead, core, trail string) {
	i, j := 0, len(text)
	for i < j && !isAlnum(text[i]) {
		i++
	}
	for j > i && !isAlnum(text[j-1]) {
		j--
	}
	return text[:i], text[i:j], text[j:]
}

// shapeSignature describes a text's structure: token kinds, the punctuation
// itself, and the widths of digit runs. Texts of the same kind of value share
// one, whatever the digits and names in them.
func shapeSignature(text string) string {
	var b strings.Builder
	for _, t := range tokenize(text) {
		switch t.kind {
		case tokDigits:
			b.WriteString("d" + string(rune('0'+len(t.text)%10)))
		case tokLetters:
			b.WriteString("a")
		default:
			b.WriteString("p" + t.text)
		}
	}
	return b.String()
}

// majorityShapeGroup returns the texts that share the most common shape
// (ignoring the punctuation around them), or nil if there are none. Lines
// that are something else entirely (a continuation, a stack frame) fall out.
func majorityShapeGroup(texts []string) []string {
	groups := map[string][]string{}
	order := []string{}
	for _, t := range texts {
		_, core, _ := frameOf(t)
		sig := shapeSignature(core)
		if _, ok := groups[sig]; !ok {
			order = append(order, sig)
		}
		groups[sig] = append(groups[sig], t)
	}
	var best []string
	for _, sig := range order {
		if len(groups[sig]) > len(best) {
			best = groups[sig]
		}
	}
	return best
}

// commonFrame returns the punctuation that opens and closes every one of the
// texts: the longest common prefix and suffix made only of non-alphanumeric
// bytes. Never more than that, so no part of a value is mistaken for frame.
func commonFrame(texts []string) (prefix, suffix string) {
	if len(texts) == 0 {
		return "", ""
	}
	lead, _, trail := frameOf(texts[0])
	for _, t := range texts[1:] {
		l, _, tr := frameOf(t)
		lead = commonPrefix(lead, l)
		trail = commonSuffix(trail, tr)
	}
	return lead, trail
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

func commonSuffix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return a[len(a)-n:]
}

// fieldWindowTexts returns, for each line that has them, the text of span
// whitespace-delimited fields starting at idx (1-based from the front, or
// negative from the end), joined by single spaces.
func fieldWindowTexts(lines []string, idx, span int) []string {
	var out []string
	for _, line := range lines {
		fields := strings.Fields(line)
		i := idx - 1
		if idx < 0 {
			i = len(fields) + idx
		}
		if i >= 0 && i+span <= len(fields) {
			out = append(out, strings.Join(fields[i:i+span], " "))
		}
	}
	return out
}

// derivedFrame is the ExtractSpec that strips the punctuation common to every
// line's text at a field window, learned from those lines. It never names a
// character in advance: a window whose lines disagree about their punctuation
// strips nothing.
func derivedFrame(lines []string, idx, span int) *ExtractSpec {
	prefix, suffix := commonFrame(fieldWindowTexts(lines, idx, span))
	return &ExtractSpec{Prefix: prefix, Suffix: suffix}
}
