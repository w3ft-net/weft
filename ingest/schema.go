package ingest

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Schema describes how to parse raw log lines for an app.
type Schema struct {
	App      string `yaml:"app"`
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

// LoadSchema reads a schema YAML file.
func LoadSchema(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Schema
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save writes the schema to a YAML file.
func (s *Schema) Save(path string) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
