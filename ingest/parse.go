package ingest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Parser applies a schema to raw log lines, producing LogRow structs.
type Parser struct {
	schema  *Schema
	levelRe *regexp.Regexp
	traceRe *regexp.Regexp

	spanNameRe   *regexp.Regexp
	spanIDRe     *regexp.Regexp
	parentSpanRe *regexp.Regexp
	durationNsRe *regexp.Regexp

	// JSON paths, compiled once here instead of per row — mirrors the
	// *Re fields above. nil means the corresponding schema field isn't
	// JSON-path mode (positional, disabled, or a whole-line pattern
	// search). hasJSONPaths gates whether ParseLine bothers parsing the
	// JSON body at all.
	timestampPath  []jsonPathSegment
	levelPath      []jsonPathSegment
	tracePath      []jsonPathSegment
	spanNamePath   []jsonPathSegment
	spanIDPath     []jsonPathSegment
	parentSpanPath []jsonPathSegment
	durationNsPath []jsonPathSegment
	hasJSONPaths   bool
}

// NewParser creates a parser for the given schema.
func NewParser(s *Schema) (*Parser, error) {
	p := &Parser{schema: s}
	if s.LevelPattern != "" {
		re, err := regexp.Compile(s.LevelPattern)
		if err != nil {
			return nil, fmt.Errorf("compile level pattern: %w", err)
		}
		p.levelRe = re
	}
	if s.TraceIDPattern != "" {
		re, err := regexp.Compile(s.TraceIDPattern)
		if err != nil {
			return nil, fmt.Errorf("compile trace ID pattern: %w", err)
		}
		p.traceRe = re
	}
	for pattern, dst := range map[string]**regexp.Regexp{
		s.SpanNamePattern:     &p.spanNameRe,
		s.SpanIDPattern:       &p.spanIDRe,
		s.ParentSpanIDPattern: &p.parentSpanRe,
		s.DurationNsPattern:   &p.durationNsRe,
	} {
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile span pattern %q: %w", pattern, err)
		}
		*dst = re
	}

	for name, spec := range map[string]struct {
		field string
		dst   *[]jsonPathSegment
	}{
		"timestamp_field":      {s.TimestampField, &p.timestampPath},
		"level_field":          {s.LevelField, &p.levelPath},
		"trace_id_field":       {s.TraceIDField, &p.tracePath},
		"span_name_field":      {s.SpanNameField, &p.spanNamePath},
		"span_id_field":        {s.SpanIDField, &p.spanIDPath},
		"parent_span_id_field": {s.ParentSpanIDField, &p.parentSpanPath},
		"duration_ns_field":    {s.DurationNsField, &p.durationNsPath},
	} {
		if !strings.HasPrefix(spec.field, ".") {
			continue
		}
		segs, err := compileJSONPath(spec.field)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", name, err)
		}
		*spec.dst = segs
		p.hasJSONPaths = true
	}

	return p, nil
}

// ParseLine parses a single raw log line into a LogRow.
// ingestTime is the time the line was received (used as fallback timestamp).
func (p *Parser) ParseLine(raw string, ingestTime time.Time) (*pb.LogRow, error) {
	row := &pb.LogRow{
		Raw:        raw,
		Service:    p.schema.Service,
		CustomerId: p.schema.CustomerID,
		TsUnixNs:   ingestTime.UnixNano(),
	}

	// Strip envelope.
	body, instance := p.stripEnvelope(raw)
	row.Instance = instance

	// Split into fields, for the schema's positional extractors. The
	// shard stores its own split, awk's, in the fields column.
	fields := p.splitFields(body)

	// Parse the JSON body (if any) once and share it across every
	// JSON-path extractor below, instead of each one re-unmarshaling the
	// same bytes — this schema can have up to 7 JSON-path fields, and
	// this is the actual per-row ingest hot path.
	var jsonBody any
	var hasJSONBody bool
	if p.hasJSONPaths {
		jsonBody, hasJSONBody = parseJSONBody(body)
	}

	// Extract timestamp.
	if ts, ok := p.extractTimestamp(fields, body, jsonBody, hasJSONBody); ok {
		row.TsUnixNs = ts.UnixNano()
	}

	// Extract level.
	row.Level = p.extractLevel(fields, body, jsonBody, hasJSONBody)

	// Extract trace ID.
	row.TraceId = p.extractTraceID(fields, body, jsonBody, hasJSONBody)

	// Extract span fields.
	row.SpanName = p.extractSpanField(fields, body, p.schema.SpanNameField, p.spanNameRe, p.spanNamePath, jsonBody, hasJSONBody)
	row.SpanId = p.extractSpanField(fields, body, p.schema.SpanIDField, p.spanIDRe, p.spanIDPath, jsonBody, hasJSONBody)
	row.ParentSpanId = p.extractSpanField(fields, body, p.schema.ParentSpanIDField, p.parentSpanRe, p.parentSpanPath, jsonBody, hasJSONBody)
	if s := p.extractSpanField(fields, body, p.schema.DurationNsField, p.durationNsRe, p.durationNsPath, jsonBody, hasJSONBody); s != "" {
		if ns, err := strconv.ParseInt(s, 10, 64); err == nil {
			row.DurationNs = ns
		}
	}

	// Extract JSON payload.
	row.PayloadJson = p.extractPayload(fields, body)

	return row, nil
}

// ParseLines parses multiple raw lines.
func (p *Parser) ParseLines(lines []string, ingestTime time.Time) ([]*pb.LogRow, error) {
	var rows []*pb.LogRow
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "━") {
			continue
		}
		row, err := p.ParseLine(line, ingestTime)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (p *Parser) stripEnvelope(line string) (body, instance string) {
	switch p.schema.Envelope {
	case "kevcons":
		if m := kevconsEnvelopeRe.FindStringSubmatch(line); m != nil {
			return line[len(m[0]):], m[2]
		}
	}
	return line, ""
}

func (p *Parser) splitFields(body string) []string {
	sep := p.schema.FieldSeparator
	if sep == "" || sep == " " {
		return strings.Fields(body)
	}
	return strings.Split(body, sep)
}

func (p *Parser) extractTimestamp(fields []string, body string, jsonBody any, hasJSONBody bool) (time.Time, bool) {
	tsField := p.schema.TimestampField
	if tsField == "" || tsField == "none" {
		return time.Time{}, false
	}

	format := p.schema.TimestampFormat
	if format == "" {
		return time.Time{}, false
	}

	// JSON path.
	if strings.HasPrefix(tsField, ".") {
		str, ok := jsonStringAt(jsonBody, hasJSONBody, p.timestampPath)
		if !ok {
			return time.Time{}, false
		}
		ts, err := time.Parse(format, str)
		if err != nil {
			return time.Time{}, false
		}
		return ts, true
	}

	// Field index.
	idx, err := strconv.Atoi(tsField)
	if err != nil || idx < 1 {
		return time.Time{}, false
	}
	idx-- // 0-based

	spans := p.schema.TimestampFields
	if spans < 1 {
		spans = 1
	}

	if idx+spans > len(fields) {
		return time.Time{}, false
	}

	val := strings.Join(fields[idx:idx+spans], " ")
	// Strip brackets (common in CLF, Celery).
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	val = strings.TrimSuffix(val, ":")

	ts, err := time.Parse(format, val)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func (p *Parser) extractLevel(fields []string, body string, jsonBody any, hasJSONBody bool) string {
	levelField := p.schema.LevelField
	if levelField == "" || levelField == "none" {
		return ""
	}

	// JSON path.
	if strings.HasPrefix(levelField, ".") {
		if str, ok := jsonStringAt(jsonBody, hasJSONBody, p.levelPath); ok {
			return strings.ToUpper(str)
		}
		return ""
	}

	idx, err := strconv.Atoi(levelField)
	if err != nil || idx < 1 || idx > len(fields) {
		return ""
	}

	val := fields[idx-1]
	val = strings.Trim(val, "[]():")

	if p.levelRe != nil {
		if m := p.levelRe.FindStringSubmatch(val); len(m) > 1 {
			return strings.ToUpper(m[1])
		}
	}

	return strings.ToUpper(val)
}

func (p *Parser) extractTraceID(fields []string, body string, jsonBody any, hasJSONBody bool) string {
	traceField := p.schema.TraceIDField
	if traceField == "" || traceField == "none" {
		// No field to anchor on — if a pattern is set, search the whole
		// line instead. The only way to pull a "key=value" trailing tag
		// out of a line whose earlier field count varies (a SQL string,
		// a request path, ...), where no single field index could ever
		// be trace_id= reliably. See TraceIDPattern's doc comment.
		if p.traceRe != nil {
			if m := p.traceRe.FindStringSubmatch(body); len(m) > 1 {
				return m[1]
			}
		}
		return ""
	}

	// JSON path.
	if strings.HasPrefix(traceField, ".") {
		str, _ := jsonStringAt(jsonBody, hasJSONBody, p.tracePath)
		return str
	}

	idx, err := strconv.Atoi(traceField)
	if err != nil || idx < 1 || idx > len(fields) {
		return ""
	}

	val := fields[idx-1]
	val = strings.Trim(val, "[]():")

	if p.traceRe != nil {
		if m := p.traceRe.FindStringSubmatch(val); len(m) > 1 {
			return m[1]
		}
		// Unlike extractLevel's fallback-to-whole-field: a trace ID only
		// has value as an exact match against another line's trace ID.
		// The raw field text ("trace_id=abc123", say) will never equal
		// the bare IDs other rows extracted, so a non-match here is
		// worse than nothing — it fabricates a row that looks traced
		// but can't actually correlate with anything.
		return ""
	}

	return val
}

// extractSpanField is extractTraceID's exact shape, generalized over
// which field/pattern/JSON-path triple to use — span_name/span_id/
// parent_span_id/duration_ns all follow the same "field index or JSON
// path, optionally refined by a pattern" rule TraceIDField/TraceIDPattern
// established. re is nil when the corresponding *Pattern is unset; path
// is nil when fieldSpec isn't JSON-path mode.
func (p *Parser) extractSpanField(fields []string, body, fieldSpec string, re *regexp.Regexp, path []jsonPathSegment, jsonBody any, hasJSONBody bool) string {
	if fieldSpec == "" || fieldSpec == "none" {
		if re != nil {
			if m := re.FindStringSubmatch(body); len(m) > 1 {
				return m[1]
			}
		}
		return ""
	}

	if strings.HasPrefix(fieldSpec, ".") {
		str, _ := jsonStringAt(jsonBody, hasJSONBody, path)
		return str
	}

	idx, err := strconv.Atoi(fieldSpec)
	if err != nil || idx < 1 || idx > len(fields) {
		return ""
	}

	val := fields[idx-1]
	val = strings.Trim(val, "[]():")

	if re != nil {
		if m := re.FindStringSubmatch(val); len(m) > 1 {
			return m[1]
		}
		return ""
	}

	return val
}

func (p *Parser) extractPayload(fields []string, body string) json.RawMessage {
	switch p.schema.Format {
	case "json":
		trimmed := strings.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			return json.RawMessage(trimmed)
		}
	case "prefix_json":
		startField := p.schema.JSONStartField
		if startField < 1 {
			return nil
		}
		// Find the byte offset where the Nth field begins.
		idx := strings.Index(body, "{")
		if idx >= 0 {
			tail := body[idx:]
			var js json.RawMessage
			if json.Unmarshal([]byte(tail), &js) == nil {
				return js
			}
		}
	}
	return nil
}
