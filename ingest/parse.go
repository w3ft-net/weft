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
	return p, nil
}

// ParseLine parses a single raw log line into a LogRow.
// ingestTime is the time the line was received (used as fallback timestamp).
func (p *Parser) ParseLine(raw string, ingestTime time.Time) (*pb.LogRow, error) {
	row := &pb.LogRow{
		Raw:      raw,
		Service:  p.schema.Service,
		Customer: p.schema.Customer,
		TsUnixNs: ingestTime.UnixNano(),
	}

	// Strip envelope.
	body, instance := p.stripEnvelope(raw)
	row.Instance = instance

	// Split into fields.
	fields := p.splitFields(body)
	row.Fields = fields

	// Extract timestamp.
	if ts, ok := p.extractTimestamp(fields, body); ok {
		row.TsUnixNs = ts.UnixNano()
	}

	// Extract level.
	row.Level = p.extractLevel(fields, body)

	// Extract trace ID.
	row.TraceId = p.extractTraceID(fields, body)

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

func (p *Parser) extractTimestamp(fields []string, body string) (time.Time, bool) {
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
		return p.extractJSONTimestamp(body, tsField, format)
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

func (p *Parser) extractJSONTimestamp(body, path, format string) (time.Time, bool) {
	// Find JSON in the body.
	idx := strings.Index(body, "{")
	if idx < 0 {
		return time.Time{}, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body[idx:]), &obj); err != nil {
		return time.Time{}, false
	}
	key := strings.TrimPrefix(path, ".")
	val, ok := obj[key]
	if !ok {
		return time.Time{}, false
	}
	str, ok := val.(string)
	if !ok {
		return time.Time{}, false
	}
	ts, err := time.Parse(format, str)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func (p *Parser) extractLevel(fields []string, body string) string {
	levelField := p.schema.LevelField
	if levelField == "" || levelField == "none" {
		return ""
	}

	// JSON path.
	if strings.HasPrefix(levelField, ".") {
		return p.extractJSONLevel(body, levelField)
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

func (p *Parser) extractJSONLevel(body, path string) string {
	idx := strings.Index(body, "{")
	if idx < 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body[idx:]), &obj); err != nil {
		return ""
	}
	key := strings.TrimPrefix(path, ".")
	val, ok := obj[key]
	if !ok {
		return ""
	}
	str, ok := val.(string)
	if !ok {
		return ""
	}
	return strings.ToUpper(str)
}

func (p *Parser) extractTraceID(fields []string, body string) string {
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
		return p.extractJSONTraceID(body, traceField)
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

func (p *Parser) extractJSONTraceID(body, path string) string {
	idx := strings.Index(body, "{")
	if idx < 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body[idx:]), &obj); err != nil {
		return ""
	}
	key := strings.TrimPrefix(path, ".")
	val, ok := obj[key]
	if !ok {
		return ""
	}
	str, ok := val.(string)
	if !ok {
		return ""
	}
	return str
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
