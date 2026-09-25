package ingest

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// kevcons envelope pattern: [YYYY-MM-DD HH:MM:SS] [i-xxxxx]
var kevconsEnvelopeRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(i-[a-f0-9]+)\] `)

// Detection holds the results of analyzing a sample of log lines.
type Detection struct {
	// Lines is the sample after stripping the first header line (if present).
	Lines []string

	// HasKevconsEnvelope is true if most lines start with [ts] [instance].
	HasKevconsEnvelope bool

	// StrippedLines are the lines with the kevcons envelope removed.
	StrippedLines []string

	// Instances are the unique instance IDs found in the envelope.
	Instances []string

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
	// levelStrings/timestampFormats are for level/timestamp.
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
}

// known log level strings
var levelStrings = map[string]bool{
	"DEBUG": true, "INFO": true, "WARN": true, "WARNING": true,
	"ERROR": true, "FATAL": true, "CRITICAL": true, "TRACE": true,
}

// common timestamp formats to try
var timestampFormats = []struct {
	layout string
	spans  int // how many whitespace-delimited fields it covers
}{
	{"2006-01-02T15:04:05Z07:00", 1},     // ISO 8601
	{"2006-01-02T15:04:05.000Z07:00", 1}, // ISO 8601 with millis
	{"2006-01-02T15:04:05", 1},           // ISO 8601 no tz
	{"2006/01/02 15:04:05", 2},           // Go default
	{"2006-01-02 15:04:05,000", 2},       // Python with millis (comma)
	{"2006-01-02 15:04:05.000", 2},       // Python with millis (dot)
	{"2006-01-02 15:04:05", 2},           // YYYY-MM-DD HH:MM:SS
	{"02/Jan/2006:15:04:05 -0700", 2},    // CLF (inside brackets)
	{"Jan 02 15:04:05", 3},               // syslog
}

// celery level pattern: "INFO/MainProcess" → extract "INFO"
var celeryLevelRe = regexp.MustCompile(`^(\w+)/\w+\]?$`)

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

	// Check for kevcons envelope.
	d.detectEnvelope()

	// Analyze the stripped lines.
	stripped := d.StrippedLines
	if len(stripped) == 0 {
		stripped = d.Lines
	}

	d.detectJSON(stripped)
	d.detectTimestamps(stripped)
	d.detectLevels(stripped)

	return d
}

func (d *Detection) detectEnvelope() {
	matches := 0
	eligible := 0 // lines that could potentially have an envelope (non-blank, non-continuation)
	instanceSet := map[string]bool{}

	for _, line := range d.Lines {
		// Continuation lines (indented, or not starting with [) don't count against the ratio.
		isContinuation := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')

		if m := kevconsEnvelopeRe.FindStringSubmatch(line); m != nil {
			matches++
			eligible++
			instanceSet[m[2]] = true
			stripped := line[len(m[0]):]
			d.StrippedLines = append(d.StrippedLines, stripped)
		} else {
			if !isContinuation {
				eligible++
			}
			d.StrippedLines = append(d.StrippedLines, line)
		}
	}

	if eligible > 0 && float64(matches)/float64(eligible) > 0.5 {
		d.HasKevconsEnvelope = true
		for inst := range instanceSet {
			d.Instances = append(d.Instances, inst)
		}
	}
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

func (d *Detection) detectTimestamps(lines []string) {
	if len(lines) == 0 {
		return
	}

	// Check each format against the first few fields of each line.
	sample := lines
	if len(sample) > 20 {
		sample = sample[:20]
	}

	type candidate struct {
		fieldIdx int
		spans    int
		format   string
		hits     int
		sample   string
	}
	var candidates []candidate

	for _, tf := range timestampFormats {
		hits := 0
		var sampleVal string

		for _, line := range sample {
			fields := strings.Fields(line)
			// Try each possible starting field position.
			maxStart := len(fields) - tf.spans
			if maxStart > 6 {
				maxStart = 6 // don't look too deep
			}
			for fi := 0; fi < maxStart; fi++ {
				val := strings.Join(fields[fi:fi+tf.spans], " ")
				// Strip surrounding brackets for CLF-style timestamps.
				val = strings.TrimPrefix(val, "[")
				val = strings.TrimRight(val, "]:")
				if _, err := time.Parse(tf.layout, val); err == nil {
					hits++
					if sampleVal == "" {
						sampleVal = val
					}
					break
				}
			}
		}

		if hits > len(sample)/2 {
			// Find which field it matched most consistently.
			fieldHits := map[int]int{}
			for _, line := range sample {
				fields := strings.Fields(line)
				maxStart := len(fields) - tf.spans
				if maxStart > 6 {
					maxStart = 6
				}
				for fi := 0; fi < maxStart; fi++ {
					val := strings.Join(fields[fi:fi+tf.spans], " ")
					val = strings.TrimPrefix(val, "[")
					val = strings.TrimSuffix(val, "]")
					if _, err := time.Parse(tf.layout, val); err == nil {
						fieldHits[fi+1]++
						break
					}
				}
			}
			bestField := 0
			bestHits := 0
			for f, h := range fieldHits {
				if h > bestHits {
					bestHits = h
					bestField = f
				}
			}
			candidates = append(candidates, candidate{
				fieldIdx: bestField,
				spans:    tf.spans,
				format:   tf.layout,
				hits:     hits,
				sample:   sampleVal,
			})
		}
	}

	for _, c := range candidates {
		d.TimestampCandidates = append(d.TimestampCandidates, TimestampCandidate{
			FieldIndex: c.fieldIdx,
			SpanFields: c.spans,
			Format:     c.format,
			Sample:     c.sample,
		})
	}
}

func (d *Detection) detectLevels(lines []string) {
	if len(lines) == 0 {
		return
	}

	sample := lines
	if len(sample) > 30 {
		sample = sample[:30]
	}

	// Check each field position for level-like values.
	type levelHit struct {
		fieldIdx int
		pattern  string // "" for whole field, regex for partial
		hits     int
		sample   string
	}

	maxFields := 10
	hits := make([]levelHit, maxFields)
	for i := range hits {
		hits[i].fieldIdx = i + 1
	}

	for _, line := range sample {
		fields := strings.Fields(line)
		for fi := 0; fi < len(fields) && fi < maxFields; fi++ {
			val := fields[fi]
			// Strip brackets/colons.
			cleaned := strings.Trim(val, "[]():")
			upper := strings.ToUpper(cleaned)

			if levelStrings[upper] {
				hits[fi].hits++
				if hits[fi].sample == "" {
					hits[fi].sample = val
				}
				continue
			}

			// Check Celery-style "INFO/MainProcess" pattern.
			if m := celeryLevelRe.FindStringSubmatch(cleaned); m != nil {
				if levelStrings[strings.ToUpper(m[1])] {
					hits[fi].hits++
					hits[fi].pattern = `^(\w+)/`
					if hits[fi].sample == "" {
						hits[fi].sample = val
					}
				}
			}
		}
	}

	for _, h := range hits {
		if h.hits > len(sample)/3 {
			d.LevelCandidates = append(d.LevelCandidates, FieldCandidate{
				FieldIndex: h.fieldIdx,
				Sample:     h.sample,
				Pattern:    h.pattern,
			})
		}
	}
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
