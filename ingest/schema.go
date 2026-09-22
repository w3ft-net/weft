package ingest

import (
	"gopkg.in/yaml.v3"
)

// Schema describes how to parse raw log lines for a service.
type Schema struct {
	// Service is the bare service name. The yaml key stays "app" for
	// compatibility with schemas already stored in log_groups.schema_yaml;
	// the poller overrides this with the log group's service before
	// parsing, so the stored value is advisory.
	Service  string `yaml:"app"`
	Customer string `yaml:"customer"`

	// Envelope is the outer wrapper applied by the transport layer.
	// "kevcons" strips the [timestamp] [instance] prefix.
	// "none" means no envelope.
	Envelope string `yaml:"envelope"`

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
	// value. E.g., for Celery's "INFO/MainProcess", pattern "^(\\w+)/"
	// extracts "INFO". If empty, the entire field value is the level.
	LevelPattern string `yaml:"level_pattern,omitempty"`

	// TraceIDField identifies which field carries a request-correlation
	// ID (e.g. an API gateway request ID, framework-generated UUID, or
	// any existing correlation token already present in the log line).
	// Same format as LevelField: 1-based field index (text/prefix_json)
	// or a JSON dot-path (json). Empty or "none" means no extraction.
	// See docs/plans/request-tracing.md.
	TraceIDField string `yaml:"trace_id_field,omitempty"`

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
