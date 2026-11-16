package ingest

import "strings"

// PreviewFields is the order values are drawn in, and the order a field wins a
// stretch of text two of them claim.
var PreviewFields = []ProbeField{
	ProbeTimestamp, ProbeLevel, ProbeTraceID, ProbeSpanName, ProbeSpanID, ProbeParentSpanID, ProbeDurationNs,
}

// ExtractorFromSchema reads a value's extractor back out of the schema, or nil
// if the schema does not extract it. The inverse of the wizard's applyExtractor.
func ExtractorFromSchema(s *Schema, f ProbeField) *ProbeExtractor {
	var ex ProbeExtractor
	switch f {
	case ProbeTimestamp:
		ex = ProbeExtractor{Field: s.TimestampField, TimestampFormat: s.TimestampFormat, TimestampFields: s.TimestampFields}
	case ProbeLevel:
		ex = ProbeExtractor{Field: s.LevelField, Pattern: s.LevelPattern}
	case ProbeTraceID:
		ex = ProbeExtractor{Field: s.TraceIDField, Pattern: s.TraceIDPattern}
	case ProbeSpanName:
		ex = ProbeExtractor{Field: s.SpanNameField, Pattern: s.SpanNamePattern}
	case ProbeSpanID:
		ex = ProbeExtractor{Field: s.SpanIDField, Pattern: s.SpanIDPattern}
	case ProbeParentSpanID:
		ex = ProbeExtractor{Field: s.ParentSpanIDField, Pattern: s.ParentSpanIDPattern}
	case ProbeDurationNs:
		ex = ProbeExtractor{Field: s.DurationNsField, Pattern: s.DurationNsPattern, DurationUnit: s.DurationUnit}
	}
	if (ex.Field == "" || ex.Field == "none") && ex.Pattern == "" {
		return nil
	}
	if spec, ok := s.Extract[string(f)]; ok {
		ex.Spec = &spec
	}
	return &ex
}

// PreviewSegment is a stretch of a line: plain text (Field empty) or the text
// one extracted value was read from.
type PreviewSegment struct {
	Text  string     `json:"text"`
	Field ProbeField `json:"field,omitempty"`
}

// Preview is a set of lines with every value the schema extracts marked on it.
// Fields lists the values that matched somewhere, in PreviewFields order: the
// key a reader needs to tell the colors apart.
type Preview struct {
	Lines  [][]PreviewSegment `json:"lines"`
	Fields []ProbeField       `json:"fields"`
}

// SchemaPreview applies every extractor in the schema to lines and returns each
// line cut into plain and per-value segments, which put back together are the
// line itself. A value on a line it is absent from simply is not marked there.
func SchemaPreview(s *Schema, lines []string) (*Preview, error) {
	base := ProbeBase{
		Envelope: s.Envelope, Format: s.Format,
		FieldSeparator: s.FieldSeparator, JSONStartField: s.JSONStartField,
	}
	type span struct {
		start, end int
		field      ProbeField
	}
	spans := make([][]span, len(lines))
	var found []ProbeField

	for _, f := range PreviewFields {
		ex := ExtractorFromSchema(s, f)
		if ex == nil {
			continue
		}
		res, err := Probe(ProbeRequest{Field: f, Base: base, Lines: lines, Previous: ex})
		if err != nil {
			return nil, err
		}
		hit := false
		for _, r := range res.Results {
			if !r.Matched || r.Text == "" || r.Line < 0 || r.Line >= len(lines) {
				continue
			}
			l := lines[r.Line]
			if !strings.HasPrefix(l, r.Prefix) || !strings.HasPrefix(l[len(r.Prefix):], r.Text) {
				continue
			}
			sp := span{len(r.Prefix), len(r.Prefix) + len(r.Text), f}
			overlaps := false
			for _, o := range spans[r.Line] {
				if sp.start < o.end && o.start < sp.end {
					overlaps = true
					break
				}
			}
			if !overlaps {
				spans[r.Line] = append(spans[r.Line], sp)
				hit = true
			}
		}
		if hit {
			found = append(found, f)
		}
	}

	p := &Preview{Lines: make([][]PreviewSegment, len(lines)), Fields: found}
	for i, l := range lines {
		ss := spans[i]
		for a := 1; a < len(ss); a++ { // insertion sort by start; at most seven
			for b := a; b > 0 && ss[b].start < ss[b-1].start; b-- {
				ss[b], ss[b-1] = ss[b-1], ss[b]
			}
		}
		at := 0
		for _, sp := range ss {
			if sp.start > at {
				p.Lines[i] = append(p.Lines[i], PreviewSegment{Text: l[at:sp.start]})
			}
			p.Lines[i] = append(p.Lines[i], PreviewSegment{Text: l[sp.start:sp.end], Field: sp.field})
			at = sp.end
		}
		if at < len(l) || len(ss) == 0 {
			p.Lines[i] = append(p.Lines[i], PreviewSegment{Text: l[at:]})
		}
	}
	return p, nil
}
