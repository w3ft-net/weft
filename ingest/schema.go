package ingest

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Schema describes how to parse raw log lines for a service.
type Schema struct {
	// Service is the bare service name. The yaml key stays "app" for
	// compatibility with schemas already stored in log_groups.schema_yaml;
	// the poller overrides this with the log group's service before
	// parsing, so the stored value is advisory.
	Service string `yaml:"app"`

	// Envelope is the outer wrapper applied by the transport layer.
	// A legacy envelope name (see legacy.go) strips a fixed prefix; new
	// schemas do not use one, a prefix being just fields read by position.
	// "none" means no envelope.
	Envelope string `yaml:"envelope"`

	// SDK marks a source written by the weft SDK: each line carries a header
	// with its timestamp, level, trace and span values, which the agent reads
	// into the row's own fields (docs/plans/sdk-line-header.md). Such a source
	// needs no extraction, and the wizard shows its values as derived.
	SDK bool `yaml:"sdk,omitempty"`

	// Format describes the body structure after envelope stripping.
	//   "text"        — whitespace-delimited fields, no JSON
	//   "json"        — entire line is JSON
	//   "prefix_json" — positional fields followed by a JSON tail
	Format string `yaml:"format"`

	// FieldSeparator splits positional fields. Default is whitespace.
	FieldSeparator string `yaml:"field_separator,omitempty"`

	// TimestampField identifies which field is the event timestamp.
	// For text/prefix_json: 1-based field index (after envelope stripping).
	// For json: a dot-path like ".ts" or ".timestamp".
	// Empty or "none" means use ingestion time.
	TimestampField string `yaml:"timestamp_field"`

	// TimestampFormat is a Go time.Parse layout for the timestamp field.
	// Only needed for text timestamps, not for unix epochs or JSON dates.
	TimestampFormat string `yaml:"timestamp_format,omitempty"`

	// TimestampFields is the number of positional fields the timestamp spans.
	// E.g., "2026/03/24 17:21:21" spans 2 fields. Default 1.
	TimestampFields int `yaml:"timestamp_fields,omitempty"`

	// LevelField identifies which field contains the log level.
	// Same format as TimestampField: field index or JSON dot-path.
	// Empty or "none" means no level extraction.
	LevelField string `yaml:"level_field,omitempty"`

	// LevelPattern is an optional regex to extract the level from the field
	// value. E.g., for a field "VALUE/other", pattern "^(\\w+)/"
	// extracts "INFO". If empty, the entire field value is the level.
	LevelPattern string `yaml:"level_pattern,omitempty"`

	// TraceIDField identifies which field carries a request-correlation
	// ID (e.g. an API gateway request ID, framework-generated UUID, or
	// any existing correlation token already present in the log line).
	// Same format as LevelField: 1-based field index (text/prefix_json)
	// or a JSON dot-path (json). Empty or "none" means no extraction.
	// See docs/plans/request-tracing.md.
	TraceIDField string `yaml:"trace_id_field,omitempty"`

	// TraceIDPattern is an optional regex to extract the trace ID.
	// Needed for Go's stdlib-`log`-style "key=value" trailing tags (e.g.
	// weft's own internal logging: "... trace_id=abc123"), where the
	// bare ID isn't its own field/value — pattern "trace_id=(\\S+)"
	// extracts just "abc123". Two modes, chosen by whether TraceIDField
	// is also set:
	//
	//   - TraceIDField set to a positional index: same relationship
	//     LevelPattern has to LevelField — the pattern refines that one
	//     pre-selected field. Requires the trailing tag to land at a
	//     stable field index every line, so only fits a fixed-shape log
	//     line (e.g. lambda-shard's "query completed in Xs (N rows)
	//     trace_id=...").
	//   - TraceIDField empty/"none": the pattern searches the *whole*
	//     line instead. The only way to extract a trailing tag from a
	//     line whose earlier field count varies (a SQL string, a request
	//     path, ...) — e.g. coordinator's own query-audit log or api's
	//     access log, where nothing before "trace_id=" sits at a fixed
	//     index. Harmlessly finds nothing on a line with no tag at all.
	//
	// Only applied on the positional/whole-line path, never the JSON
	// dot-path one (same asymmetry LevelPattern has) — a JSON field is
	// already a precise value with nothing to strip.
	TraceIDPattern string `yaml:"trace_id_pattern,omitempty"`

	// SpanNameField/SpanIDField/ParentSpanIDField/DurationNsField extract
	// weft-go's own "span start"/"span done"/"span error" lines (see
	// weft-go's span.go) into reserved columns — same convention as
	// TraceIDField: 1-based field index (text/prefix_json) or a JSON
	// dot-path (json); empty/"none" means no extraction. Each has a
	// matching *Pattern sibling for the same reason TraceIDPattern
	// exists: these are trailing "key=value" tags, not their own field,
	// so the field/pattern pair (or an empty field + whole-line pattern)
	// is what actually locates the value. See docs/plans/span-display.md.
	SpanNameField   string `yaml:"span_name_field,omitempty"`
	SpanNamePattern string `yaml:"span_name_pattern,omitempty"`

	SpanIDField   string `yaml:"span_id_field,omitempty"`
	SpanIDPattern string `yaml:"span_id_pattern,omitempty"`

	ParentSpanIDField   string `yaml:"parent_span_id_field,omitempty"`
	ParentSpanIDPattern string `yaml:"parent_span_id_pattern,omitempty"`

	// DurationNsField/DurationNsPattern extract weft-go's duration_ns=
	// tag (only present on "span done" lines) as a plain integer — see
	// span.go's End(). Unlike the other three fields, this one is
	// parsed as an integer after extraction, not stored as-is.
	DurationNsField   string `yaml:"duration_ns_field,omitempty"`
	DurationNsPattern string `yaml:"duration_ns_pattern,omitempty"`

	// DurationUnit says how the extracted duration text reads: "" or "ns" is a
	// bare integer count of nanoseconds (the original behaviour); "us", "ms"
	// and "s" are a bare, possibly fractional, number of that unit; "text" is
	// a duration written with its own units, such as 5.01s, 340ms or 1m2s.
	// The parsed value is always stored as integer nanoseconds.
	DurationUnit string `yaml:"duration_unit,omitempty"`

	// Extract holds, per value ("timestamp", "level", "trace_id", "span_name",
	// "span_id", "parent_span_id", "duration_ns"), how to read it out of its
	// field. A value with an entry here is read by the span-reporting
	// extractor (extract.go) and is stripped of exactly what its spec says:
	// the parser assumes nothing about brackets or colons. A value without an
	// entry is read the legacy way (parse.go), with that code's built-in
	// trims, so schemas stored before this existed behave exactly as before.
	// A value's *_field may then also be negative, counting fields from the
	// end of the line.
	Extract map[string]ExtractSpec `yaml:"extract,omitempty"`

	// JSONStartField is the 1-based field index where JSON begins (for
	// prefix_json format). Fields before this are positional text fields.
	// Fields from this position onward are parsed as a single JSON object.
	JSONStartField int `yaml:"json_start_field,omitempty"`

	// ExpandKeys lists JSON keys to promote to DuckDB columns for faster
	// pushdown queries.
	ExpandKeys []ExpandKey `yaml:"expand_keys,omitempty"`
}

// ExpandKey maps a JSON key to a DuckDB column type.
type ExpandKey struct {
	Key  string `yaml:"key"`
	Type string `yaml:"type"` // TEXT, INTEGER, DOUBLE, BOOLEAN
}

// LoadSchemaFromBytes parses a schema from raw YAML bytes. Used by weftd's
// parser cache and any caller that already has the schema YAML in memory.
func LoadSchemaFromBytes(data []byte) (*Schema, error) {
	var s Schema
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ToYAML returns the schema as a YAML string.
func (s *Schema) ToYAML() (string, error) {
	data, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExtractSpec says how one value is read out of the field(s) a schema points
// at. Steps apply in this order: the literal Prefix and Suffix, then the
// Chars set at both ends, then CutAt.
type ExtractSpec struct {
	// Prefix and Suffix are literal text the field must start and end with
	// ("[" in "[2026-10-15", "trace_id=" in "trace_id=abc", ":" in
	// "12:00:03,425:"); they are removed, and a field lacking them has no
	// value, so a value present on only some lines is not read from the
	// fields of the others.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Suffix string `yaml:"suffix,omitempty" json:"suffix,omitempty"`
	// Chars are ASCII characters stripped, however many, from both ends
	// (like strings.Trim). For text with a varying number of brackets or
	// parentheses around it; a literal Prefix/Suffix is safer for anything
	// that could also begin a value.
	Chars string `yaml:"chars,omitempty" json:"chars,omitempty"`
	// CutAt keeps only the text before its first occurrence ("/" turns
	// VALUE/other into VALUE).
	CutAt string `yaml:"cut_at,omitempty" json:"cut_at,omitempty"`
	// Fallback is a regex with one capture group, searched across the whole
	// line, tried only when the field-based read yields nothing.
	Fallback string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// valueNames are the keys of Schema.Extract, in valueID order.
var valueNames = [numValues]string{"timestamp", "level", "trace_id", "span_name", "span_id", "parent_span_id", "duration_ns"}

type valueID int

const (
	vTimestamp valueID = iota
	vLevel
	vTrace
	vSpanName
	vSpanID
	vParentSpanID
	vDuration
	numValues
)

// valueRef returns the schema's field reference and pattern for a value.
func (s *Schema) valueRef(id valueID) (field, pattern string) {
	switch id {
	case vTimestamp:
		return s.TimestampField, ""
	case vLevel:
		return s.LevelField, s.LevelPattern
	case vTrace:
		return s.TraceIDField, s.TraceIDPattern
	case vSpanName:
		return s.SpanNameField, s.SpanNamePattern
	case vSpanID:
		return s.SpanIDField, s.SpanIDPattern
	case vParentSpanID:
		return s.ParentSpanIDField, s.ParentSpanIDPattern
	case vDuration:
		return s.DurationNsField, s.DurationNsPattern
	}
	return "", ""
}

// MinAgentVersionForExtract is the first weftd release that can read a schema
// written with extract specs. weftd parses client-side; an older one ignores an
// `extract` section and reads a negative field index as nothing, so the values
// come out empty without any error. Schemas that use them are refused for
// sources whose agents are older (api: handleConfigureSource).
const MinAgentVersionForExtract = "0.28.0"

// MinAgentVersionForDurationUnit is the first weftd release that reads
// duration_unit; an older one parses the text as an integer count of
// nanoseconds and silently drops any other spelling.
const MinAgentVersionForDurationUnit = "0.29.0"

// MinAgentVersion is the oldest weftd that reads this schema correctly, or ""
// when any agent does.
func (s *Schema) MinAgentVersion() string {
	switch {
	case s.durationNeedsUnitParser():
		return MinAgentVersionForDurationUnit
	case s.UsesExtract():
		return MinAgentVersionForExtract
	}
	return ""
}

// durationNeedsUnitParser is whether the schema relies on duration_unit
// meaning anything beyond what every weftd already does.
func (s *Schema) durationNeedsUnitParser() bool {
	return s.DurationUnit != "" && s.DurationUnit != "ns"
}

// UsesExtract reports whether the schema needs a parser that understands
// extract specs: it has an extract section, or counts a field from the end.
func (s *Schema) UsesExtract() bool {
	if len(s.Extract) > 0 {
		return true
	}
	for id := valueID(0); id < numValues; id++ {
		if field, _ := s.valueRef(id); strings.HasPrefix(field, "-") && field != "-" {
			return true
		}
	}
	return false
}
