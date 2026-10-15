package ingest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// This file is the span-reporting extractor for values whose schema carries an
// Extract spec. It reads a value out of a line and says *where* it came from,
// in one piece of code, so what a UI highlights is by construction what the
// parser read. Values without a spec go through parse.go's legacy extractors.

// FieldSpans records, per value, the byte span of the source text the parser
// read it from, in the raw line (after any envelope). A value read the legacy
// way, or by JSON path, has no span.
type FieldSpans struct {
	s [numValues][2]int
}

func newFieldSpans() *FieldSpans {
	f := &FieldSpans{}
	for i := range f.s {
		f.s[i] = [2]int{-1, -1}
	}
	return f
}

func (f *FieldSpans) set(id valueID, start, end int) { f.s[id] = [2]int{start, end} }

// Span returns the span for the named value ("level", "trace_id", ...).
func (f *FieldSpans) Span(name string) (start, end int, ok bool) {
	for i, n := range valueNames {
		if n == name && f.s[i][0] >= 0 {
			return f.s[i][0], f.s[i][1], true
		}
	}
	return 0, 0, false
}

// Text returns the source text the named value was read from, or "".
func (f *FieldSpans) Text(raw, name string) string {
	if s, e, ok := f.Span(name); ok && e <= len(raw) {
		return raw[s:e]
	}
	return ""
}

// apply narrows [s,e) of body per the spec. ok is false when the field lacks
// the Prefix or Suffix the spec requires: such a field has no value, which is
// how a value present on only some lines is kept from being read out of the
// fields of the others.
func (sp ExtractSpec) apply(body string, s, e int) (int, int, bool) {
	if sp.Prefix != "" {
		if !strings.HasPrefix(body[s:e], sp.Prefix) {
			return s, e, false
		}
		s += len(sp.Prefix)
	}
	if sp.Suffix != "" {
		if !strings.HasSuffix(body[s:e], sp.Suffix) {
			return s, e, false
		}
		e -= len(sp.Suffix)
	}
	if sp.Chars != "" {
		for s < e && strings.IndexByte(sp.Chars, body[s]) >= 0 {
			s++
		}
		for e > s && strings.IndexByte(sp.Chars, body[e-1]) >= 0 {
			e--
		}
	}
	if sp.CutAt != "" {
		if k := strings.Index(body[s:e], sp.CutAt); k >= 0 {
			e = s + k
		}
	}
	return s, e, true
}

// compiledValue is one value's extractor, built once per schema.
type compiledValue struct {
	id       valueID
	spec     ExtractSpec
	whole    bool // no field: search the whole line with pattern
	json     []jsonPathSegment
	idx      int // positional: >0 from the front, <0 from the end (1-based)
	span     int // fields joined (timestamps)
	pattern  *regexp.Regexp
	fallback *regexp.Regexp
}

func compileValue(s *Schema, id valueID, spec ExtractSpec, jsonPath []jsonPathSegment) (*compiledValue, error) {
	field, pattern := s.valueRef(id)
	cv := &compiledValue{id: id, spec: spec, span: 1, json: jsonPath}
	if id == vTimestamp && s.TimestampFields > 1 {
		cv.span = s.TimestampFields
	}
	switch {
	case field == "" || field == "none" || field == "0":
		cv.whole = true
	case strings.HasPrefix(field, "."):
		// compiled by the caller
	default:
		n, err := strconv.Atoi(field)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("%s_field: %q is not a JSON path or a non-zero field index", valueNames[id], field)
		}
		cv.idx = n
	}
	var err error
	if pattern != "" {
		if cv.pattern, err = regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("%s pattern: %w", valueNames[id], err)
		}
	}
	if spec.Fallback != "" {
		if cv.fallback, err = regexp.Compile(spec.Fallback); err != nil {
			return nil, fmt.Errorf("%s fallback: %w", valueNames[id], err)
		}
	}
	return cv, nil
}

// fieldOffsets splits body into fields for the plan's positional values:
// into buf's storage if it fits, on a literal separator if the schema names
// one, and no further than any extractor needs.
func (p *Parser) fieldOffsets(body string, buf [][2]int) [][2]int {
	if sep := p.schema.FieldSeparator; sep != "" && sep != " " {
		return splitOffsetsSep(body, sep, buf)
	}
	return splitOffsets(body, buf, p.splitLimit())
}

// splitLimit is how many fields the split must produce: all of them if any
// extractor counts from the end (or the test hook asks), otherwise just
// enough for the highest front index in use.
func (p *Parser) splitLimit() int {
	if p.forceFullSplit {
		return 0
	}
	return p.maxFront
}

// extract reads the value from the line. It returns the text, and the span it
// came from in body (-1,-1 when the source has no position, as for a JSON
// path). Empty text means nothing was found.
func (cv *compiledValue) extract(body string, offs [][2]int, jsonBody any, hasJSON bool) (text string, s, e int) {
	s, e = -1, -1
	switch {
	case cv.json != nil:
		text, _ = jsonStringAt(jsonBody, hasJSON, cv.json)
	case cv.whole:
		if cv.pattern != nil {
			text, s, e = matchSpan(cv.pattern, body, 0)
		}
	default:
		i := cv.idx - 1
		if cv.idx < 0 {
			i = len(offs) + cv.idx
		}
		if i >= 0 && i+cv.span <= len(offs) {
			s, e = offs[i][0], offs[i+cv.span-1][1]
			var ok bool
			if s, e, ok = cv.spec.apply(body, s, e); ok {
				text = body[s:e]
				if cv.pattern != nil {
					text, s, e = matchSpan(cv.pattern, body[s:e], s)
				}
			} else {
				s, e = -1, -1
			}
		}
	}
	if text == "" && cv.fallback != nil {
		text, s, e = matchSpan(cv.fallback, body, 0)
	}
	if text == "" {
		return "", -1, -1
	}
	return text, s, e
}

// matchSpan returns the first capture group of re in s and its span in the
// enclosing string (base is s's own offset there), or "" if re does not match.
func matchSpan(re *regexp.Regexp, s string, base int) (string, int, int) {
	m := re.FindStringSubmatchIndex(s)
	if len(m) < 4 || m[2] < 0 {
		return "", -1, -1
	}
	return s[m[2]:m[3]], base + m[2], base + m[3]
}

// normalizeSpaces collapses whitespace runs to single spaces when joined
// fields were separated irregularly, since a time layout has single spaces.
func normalizeSpaces(text string) string {
	if !strings.Contains(text, "  ") && !strings.ContainsAny(text, "\t\n\r\v\f") {
		return text
	}
	return strings.Join(strings.Fields(text), " ")
}

// splitOffsets records each whitespace-delimited field's [start,end) in body,
// stopping after limit fields when limit > 0. It matches strings.Fields
// (including non-ASCII whitespace) but takes a byte loop for ASCII, which is
// where nearly every log line lives. buf is reused if it has the capacity.
func splitOffsets(body string, buf [][2]int, limit int) [][2]int {
	offs := buf[:0]
	start := -1
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c >= 0x80 {
			return splitOffsetsRunes(body, offs[:0], limit)
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
			if start >= 0 {
				offs = append(offs, [2]int{start, i})
				start = -1
				if limit > 0 && len(offs) >= limit {
					return offs
				}
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		offs = append(offs, [2]int{start, len(body)})
	}
	return offs
}

func splitOffsetsRunes(body string, offs [][2]int, limit int) [][2]int {
	start := -1
	for i, r := range body {
		if unicode.IsSpace(r) {
			if start >= 0 {
				offs = append(offs, [2]int{start, i})
				start = -1
				if limit > 0 && len(offs) >= limit {
					return offs
				}
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		offs = append(offs, [2]int{start, len(body)})
	}
	return offs
}

// splitOffsetsSep splits on a literal separator like strings.Split.
func splitOffsetsSep(body, sep string, buf [][2]int) [][2]int {
	offs := buf[:0]
	pos := 0
	for {
		k := strings.Index(body[pos:], sep)
		if k < 0 {
			return append(offs, [2]int{pos, len(body)})
		}
		offs = append(offs, [2]int{pos, pos + k})
		pos += k + len(sep)
	}
}
