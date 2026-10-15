package ingest

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ProbeField names a value the schema wizard can extract from a log line.
type ProbeField string

const (
	ProbeTimestamp    ProbeField = "timestamp"
	ProbeLevel        ProbeField = "level"
	ProbeTraceID      ProbeField = "trace_id"
	ProbeSpanName     ProbeField = "span_name"
	ProbeSpanID       ProbeField = "span_id"
	ProbeParentSpanID ProbeField = "parent_span_id"
	ProbeDurationNs   ProbeField = "duration_ns"
)

// ProbeMark is the user's statement about one sample line: either "this text
// is the value" or, with None, "there is no value on this line". Prefix is
// the line's text before the value, so the position is unambiguous without
// both sides agreeing on a string encoding's offsets.
type ProbeMark struct {
	Line   int    `json:"line"`
	Prefix string `json:"prefix"`
	Text   string `json:"text"`
	None   bool   `json:"none"`
}

// ProbeBase is the part of a schema that decides how a line is split before
// any value is extracted: the same fields the wizard has already settled.
type ProbeBase struct {
	Envelope       string `json:"envelope"`
	Format         string `json:"format"`
	FieldSeparator string `json:"field_separator"`
	JSONStartField int    `json:"json_start_field"`
}

// ProbeExtractor is one way of pulling a value out of a line, in the same
// field/pattern vocabulary as the Schema it will be saved into.
type ProbeExtractor struct {
	Field           string `json:"field"`
	Pattern         string `json:"pattern"`
	TimestampFormat string `json:"timestamp_format,omitempty"`
	TimestampFields int    `json:"timestamp_fields,omitempty"`
	// Spec says how the field's text is stripped (see ExtractSpec); nil
	// means the legacy reading, with the parser's built-in trims.
	Spec *ExtractSpec `json:"spec,omitempty"`
}

// Describe says in words how the extractor reads its value, for display.
func (e ProbeExtractor) Describe() string {
	var parts []string
	switch {
	case e.Field == "" || e.Field == "none":
		if e.Pattern != "" {
			return "anywhere in the line, matching /" + e.Pattern + "/" + e.describeTail()
		}
		return "nowhere"
	case strings.HasPrefix(e.Field, "."):
		return "JSON field " + e.Field + e.describeTail()
	}
	n, err := strconv.Atoi(e.Field)
	if err != nil {
		return "field " + e.Field + e.describeTail()
	}
	span := max(e.TimestampFields, 1)
	switch {
	case n > 0 && span > 1:
		parts = append(parts, fmt.Sprintf("fields %d-%d", n, n+span-1))
	case n > 0:
		parts = append(parts, fmt.Sprintf("field %d", n))
	case n == -1 && span == 1:
		parts = append(parts, "last field")
	case n == -2 && span == 1:
		parts = append(parts, "second-to-last field")
	case span > 1:
		parts = append(parts, fmt.Sprintf("%d fields starting %d from the end", span, -n))
	default:
		parts = append(parts, fmt.Sprintf("field %d from the end", -n))
	}
	if e.Spec != nil {
		var strip []string
		if e.Spec.Prefix != "" {
			strip = append(strip, fmt.Sprintf("the leading %q", e.Spec.Prefix))
		}
		if e.Spec.Suffix != "" {
			strip = append(strip, fmt.Sprintf("the trailing %q", e.Spec.Suffix))
		}
		if len(strip) > 0 {
			parts = append(parts, "without "+strings.Join(strip, " and "))
		}
		if e.Spec.Chars != "" {
			parts = append(parts, fmt.Sprintf("trimming %q from both ends", e.Spec.Chars))
		}
		if e.Spec.CutAt != "" {
			parts = append(parts, fmt.Sprintf("cut at %q", e.Spec.CutAt))
		}
	}
	if e.Pattern != "" {
		parts = append(parts, "matching /"+e.Pattern+"/")
	}
	return strings.Join(parts, ", ") + e.describeTail()
}

func (e ProbeExtractor) describeTail() string {
	var out string
	if e.Spec != nil && e.Spec.Fallback != "" {
		out += ", else /" + e.Spec.Fallback + "/"
	}
	if e.TimestampFormat != "" {
		out += ", format " + e.TimestampFormat
	}
	return out
}

// ProbeRequest asks for an extractor for Field that gives each positive mark
// its text and each None mark nothing. Previous, if set, is the extractor
// being corrected: lines it already got right and nobody flagged keep their
// value where the choice allows.
type ProbeRequest struct {
	Field    ProbeField      `json:"field"`
	Base     ProbeBase       `json:"base"`
	Lines    []string        `json:"lines"`
	Marks    []ProbeMark     `json:"marks"`
	Previous *ProbeExtractor `json:"previous,omitempty"`

	// Proposer, if set, is asked for candidate extractors when the built-in
	// search finds none. Its candidates are held to exactly the same test as
	// the built-in ones: the real parser, applied to the sample, must give
	// every mark its value. A proposer can therefore widen what the probe can
	// solve but never what it will accept.
	Proposer ExtractorProposer `json:"-"`
}

// ExtractorProposer suggests candidate extractors, typically by asking a
// model. Its output is untrusted: Probe verifies every candidate itself.
type ExtractorProposer interface {
	ProposeExtractors(ctx context.Context, in ProposeInput) ([]ProbeExtractor, error)
}

// ProposeInput is what a proposer is told: the sample and what the user has
// marked on it, so it can fit the marks rather than guess blind.
type ProposeInput struct {
	Field        ProbeField
	Base         ProbeBase
	Lines        []string
	Examples     []ProbeMark     // lines where the value is exactly Text
	NoValueLines []int           // lines that must yield no value
	Previous     *ProbeExtractor // the rule being corrected, if any
}

// ProbeLineResult is what the extractor yields on one sample line. Text is
// the substring it came from and Prefix the line text before it, for
// highlighting; both are empty when it could not be located.
type ProbeLineResult struct {
	Line    int    `json:"line"`
	Matched bool   `json:"matched"`
	Value   string `json:"value"`
	Text    string `json:"text,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
}

// ProbeResult is the outcome: an extractor (nil when none satisfies the
// marks, with Message saying so) and its effect on every sample line.
type ProbeResult struct {
	Extractor *ProbeExtractor `json:"extractor"`
	// Source says where the extractor came from: "rule" (built from the
	// marks) or "model" (proposed by the model, then verified). Empty when
	// there is none, or when a previous extractor was merely evaluated.
	Source  string            `json:"source,omitempty"`
	Results []ProbeLineResult `json:"results"`
	Matched int               `json:"matched"`
	Total   int               `json:"total"`
	Message string            `json:"message,omitempty"`
}

// SchemaPatch returns the extractor as the schema/configure request keys it
// sets, so a client can merge it into the form it is building.
func (r *ProbeResult) SchemaPatch(f ProbeField) map[string]any {
	if r.Extractor == nil {
		return nil
	}
	e := r.Extractor
	// "extract" maps the value's name to its spec; a null spec clears it
	// (the client merges it into the schema's extract map).
	var spec any
	if e.Spec != nil {
		spec = e.Spec
	}
	extract := map[string]any{string(f): spec}
	if f == ProbeTimestamp {
		return map[string]any{
			"timestamp_field":  e.Field,
			"timestamp_format": e.TimestampFormat,
			"timestamp_fields": e.TimestampFields,
			"extract":          extract,
		}
	}
	return map[string]any{
		string(f) + "_field":   e.Field,
		string(f) + "_pattern": e.Pattern,
		"extract":              extract,
	}
}

var probeFieldNames = map[ProbeField]bool{
	ProbeTimestamp: true, ProbeLevel: true, ProbeTraceID: true, ProbeSpanName: true,
	ProbeSpanID: true, ProbeParentSpanID: true, ProbeDurationNs: true,
}

// probeSentinel is the ingest time handed to the parser, so a row whose
// timestamp was not extracted is recognizable.
var probeSentinel = time.Unix(0, 1)

type resolvedMark struct {
	line, s, e int // s,e: byte offsets of the value within the line's body
	text       string
	none       bool
}

type probeCtx struct {
	req    ProbeRequest
	base   *Parser // only for envelope stripping and field splitting
	bodies []string
	starts []int // offset of body within each raw line
	marks  []resolvedMark
	prev   []string
}

// Probe finds the simplest extractor consistent with the user's marks and
// applies it to every sample line using the real Parser, so what the user
// verifies is what ingest will do.
func Probe(req ProbeRequest) (*ProbeResult, error) {
	return ProbeContext(context.Background(), req)
}

// ProbeContext is Probe with a context for the proposer's call.
func ProbeContext(ctx context.Context, req ProbeRequest) (*ProbeResult, error) {
	if !probeFieldNames[req.Field] {
		return nil, fmt.Errorf("unknown field %q", req.Field)
	}
	c := &probeCtx{req: req}
	bp, err := NewParser(&Schema{Envelope: req.Base.Envelope, FieldSeparator: req.Base.FieldSeparator})
	if err != nil {
		return nil, err
	}
	c.base = bp
	for _, raw := range req.Lines {
		body, _ := bp.stripEnvelope(raw)
		c.bodies = append(c.bodies, body)
		c.starts = append(c.starts, len(raw)-len(body))
	}
	positives := 0
	for _, m := range req.Marks {
		if m.Line < 0 || m.Line >= len(req.Lines) {
			return nil, fmt.Errorf("mark refers to line %d, but the sample has %d lines", m.Line, len(req.Lines))
		}
		rm := resolvedMark{line: m.Line, none: m.None, text: m.Text}
		if !m.None {
			raw := req.Lines[m.Line]
			if m.Text == "" || !strings.HasPrefix(raw, m.Prefix) || !strings.HasPrefix(raw[len(m.Prefix):], m.Text) {
				return nil, fmt.Errorf("the marked text is not at that position in line %d", m.Line)
			}
			if len(m.Prefix) < c.starts[m.Line] {
				return nil, fmt.Errorf("the marked text in line %d is inside the envelope, which is stripped before extraction", m.Line)
			}
			rm.s = len(m.Prefix) - c.starts[m.Line]
			rm.e = rm.s + len(m.Text)
			positives++
		}
		c.marks = append(c.marks, rm)
	}
	if positives == 0 && req.Previous == nil {
		return nil, fmt.Errorf("mark at least one line that has the value")
	}
	if req.Previous != nil {
		c.prev, _ = c.eval(*req.Previous)
	}

	res := &ProbeResult{Total: len(req.Lines)}
	if positives == 0 {
		// Nothing to synthesize from: just show what Previous does.
		if c.prev == nil {
			return nil, fmt.Errorf("the previous extractor does not compile")
		}
		res.Extractor = req.Previous
		c.fillResults(res)
		return res, nil
	}
	for _, group := range c.candidateGroups() {
		if best, _ := c.pick(group); best != nil {
			res.Extractor, res.Source = best, "rule"
			c.fillResults(res)
			return res, nil
		}
	}
	if req.Proposer != nil {
		if best, _ := c.askProposer(ctx); best != nil {
			res.Extractor, res.Source = best, "model"
			c.fillResults(res)
			return res, nil
		}
	}
	res.Message = "no extraction rule matches every line you marked; check the marks, or skip this field"
	res.Results = make([]ProbeLineResult, len(req.Lines))
	for i := range res.Results {
		res.Results[i].Line = i
	}
	return res, nil
}

// pick returns the candidate in group that satisfies every mark, preferring
// the one that keeps the most of the previous extractor's work.
func (c *probeCtx) pick(group []ProbeExtractor) (*ProbeExtractor, []string) {
	var best *ProbeExtractor
	var bestVals []string
	bestScore := -1
	for _, ex := range group {
		vals, err := c.eval(ex)
		if err != nil || !c.satisfies(vals) {
			continue
		}
		if score := c.softScore(vals); score > bestScore {
			ex := ex
			best, bestVals, bestScore = &ex, vals, score
		}
	}
	return best, bestVals
}

// askProposer asks the proposer for candidates and returns the first that
// the same checker accepts.
func (c *probeCtx) askProposer(ctx context.Context) (*ProbeExtractor, []string) {
	in := ProposeInput{
		Field: c.req.Field, Base: c.req.Base, Lines: c.req.Lines, Previous: c.req.Previous,
	}
	for _, m := range c.req.Marks {
		if m.None {
			in.NoValueLines = append(in.NoValueLines, m.Line)
		} else {
			in.Examples = append(in.Examples, m)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cands, err := c.req.Proposer.ProposeExtractors(ctx, in)
	if err != nil {
		log.Printf("probe: extractor proposer failed: %v", err)
		return nil, nil
	}
	group := make([]ProbeExtractor, 0, len(cands))
	for _, ex := range cands {
		group = append(group, c.normalizeProposal(ex))
	}
	// One at a time, in the model's order: a model's first choice should win
	// over a later one that merely also fits.
	for _, ex := range group {
		if best, vals := c.pick([]ProbeExtractor{ex}); best != nil {
			return best, vals
		}
	}
	return nil, nil
}

// schemaFor is the schema fragment that makes ex extract field f.
func schemaFor(f ProbeField, base ProbeBase, ex ProbeExtractor) *Schema {
	s := &Schema{
		Envelope:       base.Envelope,
		Format:         base.Format,
		FieldSeparator: base.FieldSeparator,
		JSONStartField: base.JSONStartField,
	}
	if ex.Spec != nil {
		s.Extract = map[string]ExtractSpec{string(f): *ex.Spec}
	}
	switch f {
	case ProbeTimestamp:
		s.TimestampField, s.TimestampFormat, s.TimestampFields = ex.Field, ex.TimestampFormat, ex.TimestampFields
	case ProbeLevel:
		s.LevelField, s.LevelPattern = ex.Field, ex.Pattern
	case ProbeTraceID:
		s.TraceIDField, s.TraceIDPattern = ex.Field, ex.Pattern
	case ProbeSpanName:
		s.SpanNameField, s.SpanNamePattern = ex.Field, ex.Pattern
	case ProbeSpanID:
		s.SpanIDField, s.SpanIDPattern = ex.Field, ex.Pattern
	case ProbeParentSpanID:
		s.ParentSpanIDField, s.ParentSpanIDPattern = ex.Field, ex.Pattern
	case ProbeDurationNs:
		s.DurationNsField, s.DurationNsPattern = ex.Field, ex.Pattern
	}
	return s
}

// eval runs ex over every sample line with the real parser and returns the
// extracted value of each ("" for none).
func (c *probeCtx) eval(ex ProbeExtractor) ([]string, error) {
	return evalExtractor(c.req.Field, c.req.Base, c.req.Lines, ex, false)
}

// extracted is what an extractor read on one line: the value, and where in
// the raw line its source text sits (Start < 0: no position known, as for a
// JSON path or a value read the legacy way).
type extracted struct {
	Value      string
	Start, End int
}

// evalExtractor is the one place an extractor is applied to lines: it builds
// the schema the extractor would become, runs the real Parser, and returns
// the field's value for each line ("" for none). Probe's synthesis, the
// wizard's review, and verification of classifier proposals all go through
// it, so none can disagree with what ingest will do. strict only affects
// level extraction (see Parser.strictLevel).
func evalExtractor(f ProbeField, base ProbeBase, lines []string, ex ProbeExtractor, strict bool) ([]string, error) {
	out, err := evalExtractorSpans(f, base, lines, ex, strict)
	if err != nil {
		return nil, err
	}
	vals := make([]string, len(out))
	for i, o := range out {
		vals[i] = o.Value
	}
	return vals, nil
}

// evalExtractorSpans is evalExtractor that also returns the source span the
// parser read each value from, from the same parse, so a highlight cannot
// disagree with the extracted value.
func evalExtractorSpans(f ProbeField, base ProbeBase, lines []string, ex ProbeExtractor, strict bool) ([]extracted, error) {
	p, err := NewParser(schemaFor(f, base, ex))
	if err != nil {
		return nil, err
	}
	p.strictLevel = strict
	out := make([]extracted, len(lines))
	for i, raw := range lines {
		row, spans, err := p.ParseLineSpans(raw, probeSentinel)
		if err != nil {
			return nil, err
		}
		o := extracted{Start: -1, End: -1}
		switch f {
		case ProbeTimestamp:
			if row.TsUnixNs != probeSentinel.UnixNano() {
				o.Value = time.Unix(0, row.TsUnixNs).UTC().Format(time.RFC3339Nano)
			}
		case ProbeLevel:
			o.Value = row.Level
		case ProbeTraceID:
			o.Value = row.TraceId
		case ProbeSpanName:
			o.Value = row.SpanName
		case ProbeSpanID:
			o.Value = row.SpanId
		case ProbeParentSpanID:
			o.Value = row.ParentSpanId
		case ProbeDurationNs:
			if row.DurationNs != 0 {
				o.Value = strconv.FormatInt(row.DurationNs, 10)
			}
		}
		if o.Value != "" {
			if st, en, ok := spans.Span(string(f)); ok {
				o.Start, o.End = st, en
			}
		}
		out[i] = o
	}
	return out, nil
}

// satisfies reports whether vals give every positive mark its value and
// every None mark nothing.
func (c *probeCtx) satisfies(vals []string) bool {
	for _, m := range c.marks {
		v := vals[m.line]
		switch {
		case m.none:
			if v != "" {
				return false
			}
		case c.req.Field == ProbeTimestamp:
			if v == "" {
				return false
			}
		case c.req.Field == ProbeLevel:
			if !strings.EqualFold(v, m.text) {
				return false
			}
		default:
			if v != m.text {
				return false
			}
		}
	}
	return true
}

// softScore counts lines the previous extractor got a value for, nobody
// flagged, and vals still gets the same value for.
func (c *probeCtx) softScore(vals []string) int {
	if c.prev == nil {
		return 0
	}
	marked := map[int]bool{}
	for _, m := range c.marks {
		marked[m.line] = true
	}
	n := 0
	for i, p := range c.prev {
		if p != "" && !marked[i] && vals[i] == p {
			n++
		}
	}
	return n
}

// fillResults describes the extractor's effect on every line. The highlight
// is the source span the parser itself reported; a value with no span (a JSON
// path, or one read the legacy way) is highlighted where its text first
// appears in the line, if it does.
func (c *probeCtx) fillResults(res *ProbeResult) {
	out, err := evalExtractorSpans(c.req.Field, c.req.Base, c.req.Lines, *res.Extractor, false)
	if err != nil {
		return
	}
	for i, o := range out {
		r := ProbeLineResult{Line: i, Matched: o.Value != "", Value: o.Value}
		if r.Matched {
			res.Matched++
			raw := c.req.Lines[i]
			start, end := o.Start, o.End
			if start < 0 {
				if k := strings.Index(raw, o.Value); k >= 0 {
					start, end = k, k+len(o.Value)
				}
			}
			if start >= 0 {
				r.Text, r.Prefix = raw[start:end], raw[:start]
			}
		}
		res.Results = append(res.Results, r)
	}
}

// --- candidate generation ---------------------------------------------------

// candidateGroups lists extractor candidates, simplest first. Candidates in
// one group are alternatives for the same anchor, and the best of them (by
// how much of the previous extractor's work it keeps) wins together.
func (c *probeCtx) candidateGroups() [][]ProbeExtractor {
	if c.req.Field == ProbeTimestamp {
		return c.timestampGroups()
	}
	// Structure first: a JSON path, then a field by position with the text that
	// frames it. A regex is the last resort, for a value no position finds.
	// (Level has no whole-line mode: it is read from a field.)
	groups := c.jsonPathGroups()
	for _, shape := range c.positionalShapes(false) {
		groups = append(groups, []ProbeExtractor{shape})
	}
	if c.req.Field != ProbeLevel {
		groups = append(groups, c.wholeLineGroups()...)
	}
	return groups
}

func (c *probeCtx) positives() []resolvedMark {
	var out []resolvedMark
	for _, m := range c.marks {
		if !m.none {
			out = append(out, m)
		}
	}
	return out
}

func (c *probeCtx) isJSONFormat() bool {
	return c.req.Base.Format == "json" || c.req.Base.Format == "prefix_json"
}

// jsonPathGroups: in JSON lines, locate the value by path.
func (c *probeCtx) jsonPathGroups() [][]ProbeExtractor {
	if !c.isJSONFormat() {
		return nil
	}
	first := c.positives()[0]
	body, ok := parseJSONBody(c.bodies[first.line])
	if !ok {
		return nil
	}
	var groups [][]ProbeExtractor
	for _, p := range jsonStringPaths(body, "", first.text) {
		groups = append(groups, []ProbeExtractor{{Field: p}})
	}
	return groups
}

// jsonStringPaths returns the paths of string leaves equal to want, with
// object keys visited in sorted order so the result is deterministic.
func jsonStringPaths(v any, prefix, want string) []string {
	switch t := v.(type) {
	case string:
		if t == want && prefix != "" {
			return []string{prefix}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, jsonStringPaths(t[k], prefix+"."+k, want)...)
		}
		return out
	case []any:
		var out []string
		for i, e := range t {
			out = append(out, jsonStringPaths(e, fmt.Sprintf("%s[%d]", prefix, i), want)...)
		}
		return out
	}
	return nil
}

// fieldOffsets splits line i's body into fields the way the parser will.
func (c *probeCtx) fieldOffsets(i int) [][2]int {
	if sep := c.req.Base.FieldSeparator; sep != "" && sep != " " {
		return splitOffsetsSep(c.bodies[i], sep, nil)
	}
	return splitOffsets(c.bodies[i], nil, 0)
}

// isStructural reports whether text is only punctuation or a key= / key:
// label: the kind of text that surrounds a value the same way on every line
// ("[", "]:", "trace_id="), as opposed to content that varies.
func isStructural(text string) bool {
	if text == "" {
		return true
	}
	hasWord := false
	for i := 0; i < len(text); i++ {
		if isWordByte(text[i]) {
			hasWord = true
		}
	}
	if !hasWord {
		return true
	}
	last := text[len(text)-1]
	return (last == '=' || last == ':') && keyAnchorRe.MatchString(text)
}

// positionalShapes proposes where the marked value sits among the line's
// fields, with the text stripped from around it derived from the highlight
// itself: whatever precedes it inside its first field is the prefix, and
// whatever follows it inside its last is the suffix. Nothing here knows about
// brackets or colons; a "[" is a prefix because the user's highlight began
// after one. The field is named by its index from the front, and again from
// the end, since a trailing tag is "the last field" however long the line.
// allowSpan lets the value cover several fields (timestamps).
func (c *probeCtx) positionalShapes(allowSpan bool) []ProbeExtractor {
	pos := c.positives()
	type fit struct {
		n, a, b        int
		prefix, suffix string
	}
	fits := make([]fit, len(pos))
	for i, m := range pos {
		offs := c.fieldOffsets(m.line)
		a, b := -1, -1
		for k, o := range offs {
			if o[0] <= m.s && m.s < o[1] {
				a = k
			}
			if o[0] < m.e && m.e <= o[1] {
				b = k
			}
		}
		if a < 0 || b < a || (!allowSpan && b != a) {
			return nil
		}
		body := c.bodies[m.line]
		fits[i] = fit{n: len(offs), a: a, b: b, prefix: body[offs[a][0]:m.s], suffix: body[m.e:offs[b][1]]}
	}
	span := fits[0].b - fits[0].a + 1
	for _, f := range fits {
		if f.b-f.a+1 != span {
			return nil
		}
		if f.prefix != fits[0].prefix {
			return nil // nothing literal strips what differs from line to line
		}
	}
	specs := specsFor(fits[0].prefix, func() []string {
		out := make([]string, len(fits))
		for i, f := range fits {
			out[i] = f.suffix
		}
		return out
	}())

	var shapes []ProbeExtractor
	for _, fromEnd := range []bool{false, true} {
		idx := fits[0].a + 1
		if fromEnd {
			idx = -(fits[0].n - fits[0].a)
		}
		consistent := true
		for _, f := range fits {
			j := f.a + 1
			if fromEnd {
				j = -(f.n - f.a)
			}
			consistent = consistent && j == idx
		}
		if !consistent {
			continue
		}
		for _, spec := range specs {
			spec := spec
			ex := ProbeExtractor{Field: strconv.Itoa(idx), Spec: &spec}
			if c.req.Field == ProbeTimestamp {
				ex.TimestampFields = span
			}
			shapes = append(shapes, ex)
		}
	}
	return shapes
}

// specsFor lists the specs that strip a literal prefix and the suffixes seen
// after the marks, best first. Suffixes that are structural punctuation (or
// identical on every line) are removed literally. A suffix that is content
// beginning at a delimiter ("/other]" after a value) differs from line to
// line, so cutting at the delimiter comes first, and the literal form is
// the fallback.
func specsFor(prefix string, suffixes []string) []ExtractSpec {
	allSame := true
	for _, sfx := range suffixes {
		allSame = allSame && sfx == suffixes[0]
	}
	var out []ExtractSpec
	// A shared delimiter that starts text with content after it: cut there.
	if d := suffixes[0]; d != "" && !isWordByte(d[0]) && !isStructural(d) {
		sameDelim := true
		for _, sfx := range suffixes {
			sameDelim = sameDelim && sfx != "" && sfx[0] == d[0]
		}
		if sameDelim {
			out = append(out, ExtractSpec{Prefix: prefix, CutAt: d[:1]})
		}
	}
	if allSame {
		out = append(out, ExtractSpec{Prefix: prefix, Suffix: suffixes[0]})
	}
	return out
}

func (c *probeCtx) wholeLineGroups() [][]ProbeExtractor {
	pos := c.positives()
	texts := make([]string, len(pos))
	for i, m := range pos {
		texts[i] = m.text
	}
	first := pos[0]
	body := c.bodies[first.line]
	var groups [][]ProbeExtractor
	for _, a := range c.anchors(func(i int) string {
		return c.bodies[pos[i].line][:pos[i].s]
	}, len(pos)) {
		var g []ProbeExtractor
		for _, right := range rightOptions(body, first.e) {
			for _, class := range valueClasses(texts, rightDelim(body, first.e)) {
				g = append(g, ProbeExtractor{Field: "none", Pattern: a + "(" + class + ")" + right})
			}
		}
		groups = append(groups, g)
	}
	return groups
}

type anchor struct {
	re    string
	key   bool
	width int
}

// anchors builds regex prefixes from the text to the left of each positive
// mark (left(i)), ordered simplest first: a "key=" style label right before
// the value, then ever longer suffixes of the left context. Every
// positive's left context contributes, so an anchor common to all of them
// is among the candidates.
func (c *probeCtx) anchors(left func(i int) string, n int) []string {
	var all []anchor
	seen := map[string]bool{}
	add := func(a anchor) {
		if !seen[a.re] {
			seen[a.re] = true
			all = append(all, a)
		}
	}
	for i := 0; i < n; i++ {
		l := left(i)
		if len(l) > 48 {
			l = l[len(l)-48:]
		}
		if l == "" {
			add(anchor{re: "^"})
			continue
		}
		if k := keyAnchorRe.FindString(l); k != "" {
			add(anchor{re: literalRegex(k), key: true, width: len(k)})
		}
		for w := 1; w <= len(l); w++ {
			i0 := len(l) - w
			if !utf8.RuneStart(l[i0]) || (i0 > 0 && isWordByte(l[i0-1]) && isWordByte(l[i0])) {
				continue // never start mid-word
			}
			add(anchor{re: literalRegex(l[i0:]), width: w})
		}
		if len(left(i)) <= 48 {
			add(anchor{re: "^" + literalRegex(l), width: len(l) + 1})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key
		}
		return all[i].width < all[j].width
	})
	out := make([]string, len(all))
	for i, a := range all {
		out[i] = a.re
	}
	return out
}

var keyAnchorRe = regexp.MustCompile(`[A-Za-z_][\w.\-#@]*\s*[=:]\s*["']?$`)

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// literalRegex quotes s, letting any whitespace run match any whitespace.
func literalRegex(s string) string {
	var b strings.Builder
	for _, part := range regexp.MustCompile(`\s+|[^\s]+`).FindAllString(s, -1) {
		if strings.TrimSpace(part) == "" {
			b.WriteString(`\s+`)
		} else {
			b.WriteString(regexp.QuoteMeta(part))
		}
	}
	return b.String()
}

// rightDelim is the punctuation just after the value, if any ("]" in "[abc]").
func rightDelim(text string, e int) string {
	if e >= len(text) {
		return ""
	}
	r, _ := utf8.DecodeRuneInString(text[e:])
	if r == utf8.RuneError || strings.ContainsRune(" \t", r) || isWordByte(byte(r)) && r < 128 {
		return ""
	}
	return string(r)
}

func rightOptions(text string, e int) []string {
	if d := rightDelim(text, e); d != "" {
		return []string{regexp.QuoteMeta(d), ""}
	}
	return []string{""}
}

// valueClasses lists character classes that cover every value, narrowest
// first; delim, if set, is excluded from the catch-all classes so a value
// does not swallow its closing bracket.
func valueClasses(values []string, delim string) []string {
	all := func(re *regexp.Regexp) bool {
		for _, v := range values {
			if !re.MatchString(v) {
				return false
			}
		}
		return true
	}
	var out []string
	for _, k := range []struct{ class, test string }{
		{`\d+`, `^\d+$`},
		{`[0-9a-fA-F]+`, `^[0-9a-fA-F]+$`},
		{`[0-9a-fA-F-]+`, `^[0-9a-fA-F-]+$`},
		{`[A-Za-z]+`, `^[A-Za-z]+$`},
		{`[\w-]+`, `^[\w-]+$`},
		{`[\w.-]+`, `^[\w.-]+$`},
	} {
		if all(regexp.MustCompile(k.test)) {
			out = append(out, k.class)
		}
	}
	if delim != "" {
		out = append(out, `[^\s`+regexp.QuoteMeta(delim)+`]+`)
	}
	return append(out, `\S+`)
}

// --- timestamps -------------------------------------------------------------

func (c *probeCtx) timestampGroups() [][]ProbeExtractor {
	pos := c.positives()
	texts := make([]string, len(pos))
	for i, m := range pos {
		texts[i] = m.text
	}
	// The layout comes from the marked text itself; every other line at the
	// same place then only ranks the layouts (day-first or month-first) by
	// which reads the log in chronological order.
	layouts := inferLayouts(texts)
	if len(layouts) == 0 {
		return nil
	}
	var groups [][]ProbeExtractor
	first := pos[0]
	if c.isJSONFormat() {
		if body, ok := parseJSONBody(c.bodies[first.line]); ok {
			for _, p := range jsonStringPaths(body, "", first.text) {
				for _, layout := range layouts {
					groups = append(groups, []ProbeExtractor{{Field: p, TimestampFormat: layout}})
				}
			}
		}
	}
	for _, shape := range c.positionalShapes(true) {
		ranked := rankChronologically(append([]string(nil), layouts...), c.shapeTexts(shape))
		for _, layout := range ranked {
			ex := shape
			ex.TimestampFormat = layout
			groups = append(groups, []ProbeExtractor{ex})
		}
	}
	return groups
}

// shapeTexts is the text a positional shape reads on every line that has it.
func (c *probeCtx) shapeTexts(shape ProbeExtractor) []string {
	idx, err := strconv.Atoi(shape.Field)
	if err != nil {
		return nil
	}
	span := max(shape.TimestampFields, 1)
	var out []string
	for i := range c.req.Lines {
		offs := c.fieldOffsets(i)
		k := idx - 1
		if idx < 0 {
			k = len(offs) + idx
		}
		if k < 0 || k+span > len(offs) {
			continue
		}
		body := c.bodies[i]
		s, e := offs[k][0], offs[k+span-1][1]
		if shape.Spec != nil {
			var ok bool
			if s, e, ok = shape.Spec.apply(body, s, e); !ok {
				continue
			}
		}
		out = append(out, normalizeSpaces(body[s:e]))
	}
	return out
}

// DetectedExtractors are the extractors for every detected candidate of f, in
// the order detection ranked them (left to right for fields), so a wizard can
// offer each in turn. Frames are learned from d's lines.
func DetectedExtractors(d *Detection, f ProbeField) []*ProbeExtractor {
	if d == nil {
		return nil
	}
	var out []*ProbeExtractor
	if f == ProbeTimestamp {
		for _, t := range d.TimestampCandidates {
			ex := &ProbeExtractor{TimestampFormat: t.Format}
			if t.JSONPath != "" {
				ex.Field = t.JSONPath
			} else {
				ex.Field = strconv.Itoa(t.FieldIndex)
				ex.TimestampFields = max(t.SpanFields, 1)
				ex.Spec = &ExtractSpec{Prefix: t.Prefix, Suffix: t.Suffix}
			}
			out = append(out, ex)
		}
		return out
	}
	var cands []FieldCandidate
	switch f {
	case ProbeLevel:
		cands = d.LevelCandidates
	case ProbeTraceID:
		cands = d.TraceIDCandidates
	case ProbeSpanName:
		cands = d.SpanNameCandidates
	case ProbeSpanID:
		cands = d.SpanIDCandidates
	case ProbeParentSpanID:
		cands = d.ParentSpanIDCandidates
	case ProbeDurationNs:
		cands = d.DurationNsCandidates
	}
	for _, c := range cands {
		switch {
		case c.JSONPath != "":
			out = append(out, &ProbeExtractor{Field: c.JSONPath, Pattern: c.Pattern})
		case c.FieldIndex != 0:
			spec := derivedFrame(d.Lines, c.FieldIndex, 1)
			spec.CutAt = c.CutAt
			out = append(out, &ProbeExtractor{Field: strconv.Itoa(c.FieldIndex), Pattern: c.Pattern, Spec: spec})
		default:
			out = append(out, &ProbeExtractor{Field: "none", Pattern: c.Pattern})
		}
	}
	return out
}

// DetectedExtractor is the first detected candidate of f, or nil.
func DetectedExtractor(d *Detection, f ProbeField) *ProbeExtractor {
	if all := DetectedExtractors(d, f); len(all) > 0 {
		return all[0]
	}
	return nil
}

// normalizeProposal makes a proposed whole-line regex read the way ingest
// reads one: through its only capturing group. A proposal with several is not
// wrong, just ambiguous, and the marks settle it: the group that yields the
// marked text on every marked line is the value, and every other group is
// made non-capturing. A proposal with no such group is returned unchanged
// (and then fails verification like any other bad rule).
func (c *probeCtx) normalizeProposal(ex ProbeExtractor) ProbeExtractor {
	if (ex.Field != "none" && ex.Field != "") || ex.Pattern == "" {
		return ex
	}
	re, err := regexp.Compile(ex.Pattern)
	if err != nil || re.NumSubexp() < 2 {
		return ex
	}
	for g := 1; g <= re.NumSubexp(); g++ {
		fits := true
		for _, m := range c.positives() {
			sub := re.FindStringSubmatch(c.bodies[m.line])
			if len(sub) <= g || sub[g] != m.text {
				fits = false
				break
			}
		}
		if !fits {
			continue
		}
		if only, err := onlyCaptureGroup(ex.Pattern, g); err == nil {
			ex.Pattern = only
		}
		return ex
	}
	return ex
}

// onlyCaptureGroup rewrites pattern so group keep (1-based) is its only
// capturing group: every other capturing parenthesis becomes (?:. The text is
// otherwise untouched, so the rule reads as whoever wrote it spelled it. An
// escaped parenthesis, one inside a character class, and groups already
// non-capturing are left alone.
func onlyCaptureGroup(pattern string, keep int) (string, error) {
	var out strings.Builder
	group, inClass := 0, false
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '\\' && i+1 < len(pattern):
			out.WriteByte(c)
			i++
			out.WriteByte(pattern[i])
			continue
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '(':
			capturing := true
			if strings.HasPrefix(pattern[i:], "(?") {
				// Only named groups capture among the (?...) forms.
				capturing = strings.HasPrefix(pattern[i:], "(?P<") || strings.HasPrefix(pattern[i:], "(?<")
			}
			if capturing {
				group++
				if group != keep {
					out.WriteString("(?:")
					if strings.HasPrefix(pattern[i:], "(?") { // a named group: drop its name too
						i += strings.IndexByte(pattern[i:], '>')
					}
					continue
				}
			}
		}
		out.WriteByte(c)
	}
	res := out.String()
	re, err := regexp.Compile(res)
	if err != nil {
		return "", err
	}
	if re.NumSubexp() != 1 {
		return "", fmt.Errorf("rewritten pattern has %d capturing groups", re.NumSubexp())
	}
	return res, nil
}
