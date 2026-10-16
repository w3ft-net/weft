package ingest

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
)

// Detection holds the results of analyzing a sample of log lines.
type Detection struct {
	// Lines is the sample after stripping the first header line (if present).
	Lines []string

	// JSONCoverage is the fraction of lines that are valid JSON (after stripping).
	JSONCoverage float64

	// JSONTailCoverage is the fraction of lines where JSON starts partway through.
	JSONTailCoverage float64

	// JSONTailStartField is the most common field index where JSON begins
	// (1-based), if JSONTailCoverage is significant.
	JSONTailStartField int

	// TimestampCandidates are field indices (1-based) that look like timestamps,
	// along with their detected Go format string.
	TimestampCandidates []TimestampCandidate

	// LevelCandidates are field indices that look like log levels.
	LevelCandidates []FieldCandidate

	// TraceIDCandidates, SpanNameCandidates, SpanIDCandidates,
	// ParentSpanIDCandidates, and DurationNsCandidates mirror
	// LevelCandidates for Schema's other tag-style fields (see
	// TraceIDField/SpanNameField/etc. in schema.go). Populated by the LLM
	// classifier (ingest/llmdetect.go) — the heuristic Detect never sets
	// these; there's no fixed-vocabulary equivalent for them the way
	TraceIDCandidates      []FieldCandidate
	SpanNameCandidates     []FieldCandidate
	SpanIDCandidates       []FieldCandidate
	ParentSpanIDCandidates []FieldCandidate
	DurationNsCandidates   []FieldCandidate
}

// TimestampCandidate is a field that might be a timestamp.
type TimestampCandidate struct {
	FieldIndex int    // 1-based positional index; 0 when JSONPath is set instead
	SpanFields int    // how many fields the timestamp spans (usually 1 or 2); positional only
	Format     string // Go time.Parse layout
	JSONPath   string // JSON path (format=json, or the JSON tail of prefix_json); "" = positional
	Sample     string // example value
	// Prefix and Suffix are the punctuation that opens and closes the text on
	// every line ("[" and ":" around 2026-10-15 12:00:01,123:), to strip
	// before the layout applies.
	Prefix string
	Suffix string
}

// FieldCandidate is a field that might hold a log level, trace ID, or one
// of the span fields (span name/ID, parent span ID, duration) — the shape
// Schema's *Field/*Pattern pairs share for all of those (schema.go:66-80).
// Exactly one of FieldIndex/JSONPath is meaningful for a given candidate,
// mirroring how Schema's own *Field strings disambiguate (a leading '.'
// means JSON path, a bare positive integer means positional field index).
type FieldCandidate struct {
	FieldIndex int    // 1-based positional index; 0 = not positional (JSONPath set, or a whole-line pattern search)
	JSONPath   string // JSON path; "" = not JSON-based
	Sample     string // example value
	Pattern    string // regex to extract the value, or "" for the whole field/path value
	// CutAt, for a positional field, keeps only the text before its first
	// occurrence ("/" for "VALUE/other"): the field's head is the value.
	CutAt string
	// Unit, for a duration, is how its text reads (see Schema.DurationUnit).
	Unit string
	// Values are the distinct values seen at that position (for a categorical
	// field), so a person can judge what it is.
	Values []string
}

// Detect analyzes a sample of raw log lines and returns detection results.
func Detect(lines []string) *Detection {
	d := &Detection{}

	// Skip header and blank lines. Preserve leading whitespace (continuation markers).
	var cleanLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "━") {
			continue
		}
		cleanLines = append(cleanLines, line)
	}
	d.Lines = cleanLines

	d.detectJSON(d.Lines)
	d.detectTimestamps(d.Lines)
	d.detectCategorical(d.Lines)

	return d
}

func (d *Detection) detectJSON(lines []string) {
	if len(lines) == 0 {
		return
	}

	jsonCount := 0
	tailCounts := map[int]int{} // field index → count of lines with JSON starting there

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Full JSON line?
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			var js json.RawMessage
			if json.Unmarshal([]byte(trimmed), &js) == nil {
				jsonCount++
				continue
			}
		}

		// JSON tail? Find the first '{' and check if the rest is valid JSON.
		if idx := strings.Index(trimmed, "{"); idx > 0 {
			tail := trimmed[idx:]
			var js json.RawMessage
			if json.Unmarshal([]byte(tail), &js) == nil {
				// Count which field index this corresponds to.
				prefix := trimmed[:idx]
				fieldIdx := len(strings.Fields(prefix)) + 1
				tailCounts[fieldIdx]++
			}
		}
	}

	d.JSONCoverage = float64(jsonCount) / float64(len(lines))

	// Find the most common JSON tail start field.
	bestField := 0
	bestCount := 0
	totalTail := 0
	for field, count := range tailCounts {
		totalTail += count
		if count > bestCount {
			bestCount = count
			bestField = field
		}
	}
	if len(lines) > 0 {
		d.JSONTailCoverage = float64(totalTail) / float64(len(lines))
	}
	if bestCount > 0 {
		d.JSONTailStartField = bestField
	}
}

// detectTimestamps looks for a timestamp by position: for each starting field
// and each span of 1-4 fields, the lines' text is grouped by shape, a layout
// is inferred from the majority shape (see inferLayouts), and the punctuation
// common to those lines around it is reported as the frame. Nothing about any
// particular format is known: a layout is offered only if time.Parse accepts
// every line of the majority with it and it names a full date.
func (d *Detection) detectTimestamps(lines []string) {
	sample := make([][]string, 0, 20)
	for _, line := range lines {
		if len(sample) == 20 {
			break
		}
		sample = append(sample, strings.Fields(line))
	}
	if len(sample) == 0 {
		return
	}

	// Every window that yields a layout; where several yield the same layout
	// for the same text, the tightest wins (extra fields that are only common
	// punctuation widen a window without adding anything).
	type key struct{ format, sample string }
	tightest := map[key]TimestampCandidate{}
	for start := 0; start < 6; start++ {
		for span := 1; span <= 4; span++ {
			for _, c := range timestampWindow(sample, start, span) {
				k := key{c.Format, c.Sample}
				if prev, ok := tightest[k]; !ok || c.SpanFields < prev.SpanFields ||
					(c.SpanFields == prev.SpanFields && len(c.Prefix)+len(c.Suffix) < len(prev.Prefix)+len(prev.Suffix)) {
					tightest[k] = c
				}
			}
		}
	}

	// Per starting field, the richest layout (the longest) is the candidate.
	for start := 0; start < 6; start++ {
		var best []TimestampCandidate
		bestScore := 0
		for span := 1; span <= 4; span++ {
			var here []TimestampCandidate
			for _, c := range timestampWindow(sample, start, span) {
				if t := tightest[key{c.Format, c.Sample}]; t.FieldIndex == c.FieldIndex && t.SpanFields == c.SpanFields {
					here = append(here, c)
				}
			}
			if len(here) > 0 && len(here[0].Format) > bestScore {
				best, bestScore = here, len(here[0].Format)
			}
		}
		d.TimestampCandidates = append(d.TimestampCandidates, best...)
	}
}

// timestampWindow infers timestamp layouts for the span fields starting at
// start (0-based), across the sample lines that have them.
func timestampWindow(sample [][]string, start, span int) []TimestampCandidate {
	var texts []string
	for _, fields := range sample {
		if len(fields) >= start+span {
			texts = append(texts, strings.Join(fields[start:start+span], " "))
		}
	}
	group := majorityShapeGroup(texts)
	if len(group) <= len(sample)/2 {
		return nil
	}
	prefix, suffix := commonFrame(group)
	stripped := make([]string, len(group))
	for i, t := range group {
		stripped[i] = t[len(prefix) : len(t)-len(suffix)]
	}
	var out []TimestampCandidate
	for _, layout := range inferLayouts(stripped) {
		out = append(out, TimestampCandidate{
			FieldIndex: start + 1, SpanFields: span, Format: layout,
			Sample: stripped[0], Prefix: prefix, Suffix: suffix,
		})
	}
	return out
}

// detectCategorical finds fields whose leading word comes from a small set that
// keeps recurring: the shape of a severity, a method, a state. It cannot say
// which of them is the severity (that is a judgment for the model or the person
// looking at the lines), so it offers every one, left to right, in
// LevelCandidates. What counts is structure only: the field's leading letters,
// an optional delimiter after them (and anything after the delimiter is
// ignored), between two and eight distinct values, and most lines carrying a
// value seen elsewhere in the sample.
func (d *Detection) detectCategorical(lines []string) {
	var sample [][]string
	for _, line := range lines {
		if len(sample) == 30 {
			break
		}
		sample = append(sample, strings.Fields(line))
	}
	if len(sample) == 0 {
		return
	}

	const maxFields = 10
	type seen struct {
		counts map[string]int
		first  string
	}
	byCut := make([]map[string]*seen, maxFields) // field -> delimiter -> values
	for i := range byCut {
		byCut[i] = map[string]*seen{}
	}
	for _, fields := range sample {
		for fi := 0; fi < len(fields) && fi < maxFields; fi++ {
			head, cut, ok := categoricalHead(fields[fi])
			if !ok {
				continue
			}
			sv := byCut[fi][cut]
			if sv == nil {
				sv = &seen{counts: map[string]int{}, first: fields[fi]}
				byCut[fi][cut] = sv
			}
			sv.counts[head]++
		}
	}

	for fi := 0; fi < maxFields; fi++ {
		bestCut, n := "", 0
		var best *seen
		for cut, sv := range byCut[fi] {
			total := 0
			for _, c := range sv.counts {
				total += c
			}
			if total > n || (total == n && cut < bestCut) {
				bestCut, n, best = cut, total, sv
			}
		}
		if best == nil || n <= len(sample)/3 {
			continue
		}
		if k := len(best.counts); k < 2 || k > 8 {
			continue
		}
		recurring := 0
		values := make([]string, 0, len(best.counts))
		for v, c := range best.counts {
			values = append(values, v)
			if c >= 2 {
				recurring += c
			}
		}
		if recurring*3 < n*2 {
			continue // mostly one-off words: free text, not a set
		}
		sort.Strings(values)
		d.LevelCandidates = append(d.LevelCandidates, FieldCandidate{
			FieldIndex: fi + 1, Sample: best.first, CutAt: bestCut, Values: values,
		})
	}
}

// categoricalHead splits a field the way a categorical field is read: drop the
// punctuation around it, take the leading letters, and report the delimiter
// that follows them ("" if they are the whole field). ok is false when the
// letters are followed by a digit or a non-ASCII byte, which would make it a
// different word.
func categoricalHead(field string) (head, cut string, ok bool) {
	_, core, _ := frameOf(field)
	k := 0
	for k < len(core) && (core[k] >= 'a' && core[k] <= 'z' || core[k] >= 'A' && core[k] <= 'Z') {
		k++
	}
	if k == 0 {
		return "", "", false
	}
	if k == len(core) {
		return core, "", true
	}
	if c := core[k]; c >= '0' && c <= '9' || c >= 0x80 {
		return "", "", false
	}
	return core[:k], core[k : k+1], true
}

// SuggestFormat returns the best-guess format based on detection.
func (d *Detection) SuggestFormat() string {
	if d.JSONCoverage > 0.8 {
		return "json"
	}
	if d.JSONTailCoverage > 0.5 {
		return "prefix_json"
	}
	return "text"
}

// isAlphaNumOrDash returns true if the string contains only [a-zA-Z0-9-_].
func isAlphaNumOrDash(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
