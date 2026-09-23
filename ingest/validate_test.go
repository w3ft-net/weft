package ingest

import "testing"

func TestSchema_Validate(t *testing.T) {
	cases := []struct {
		name    string
		schema  Schema
		wantErr []string // substrings expected in the returned messages, in order
	}{
		{
			name:   "empty schema is valid",
			schema: Schema{},
		},
		{
			name:   "minimal text schema with timestamp is valid",
			schema: Schema{Format: "text", TimestampField: "1", TimestampFormat: "2006/01/02 15:04:05"},
		},
		{
			name:   "json dot-path fields are valid",
			schema: Schema{Format: "json", TimestampField: ".ts", TimestampFormat: time_RFC3339, LevelField: ".level", TraceIDField: ".trace_id"},
		},
		{
			name:    "bad format",
			schema:  Schema{Format: "jsonn"},
			wantErr: []string{`format: "jsonn" is not one of text, json, prefix_json`},
		},
		{
			name:    "prefix_json without json_start_field",
			schema:  Schema{Format: "prefix_json"},
			wantErr: []string{"json_start_field: required"},
		},
		{
			name:   "prefix_json with json_start_field is valid",
			schema: Schema{Format: "prefix_json", JSONStartField: 3},
		},
		{
			name:    "bad envelope",
			schema:  Schema{Envelope: "nginx"},
			wantErr: []string{`envelope: "nginx" is not one of none, kevcons`},
		},
		{
			name:    "timestamp_field without timestamp_format",
			schema:  Schema{TimestampField: "2"},
			wantErr: []string{"timestamp_format: required"},
		},
		{
			name:    "timestamp_field not a field ref",
			schema:  Schema{TimestampField: "first"},
			wantErr: []string{`timestamp_field: "first" is neither a JSON dot-path`},
		},
		{
			name:    "timestamp_field zero index",
			schema:  Schema{TimestampField: "0", TimestampFormat: time_RFC3339},
			wantErr: []string{"timestamp_field: field index must be >= 1"},
		},
		{
			name:    "negative timestamp_fields",
			schema:  Schema{TimestampFields: -1},
			wantErr: []string{"timestamp_fields: must be >= 0"},
		},
		{
			name:    "bad level_field",
			schema:  Schema{LevelField: "x"},
			wantErr: []string{`level_field: "x" is neither a JSON dot-path`},
		},
		{
			name:    "bad trace_id_field",
			schema:  Schema{TraceIDField: "abc"},
			wantErr: []string{`trace_id_field: "abc" is neither a JSON dot-path`},
		},
		{
			name:    "invalid level_pattern regex",
			schema:  Schema{LevelPattern: "(unclosed"},
			wantErr: []string{"level_pattern:"},
		},
		{
			name:    "expand_keys missing key",
			schema:  Schema{ExpandKeys: []ExpandKey{{Type: "TEXT"}}},
			wantErr: []string{"expand_keys[0]: key is required"},
		},
		{
			name:    "expand_keys bad type",
			schema:  Schema{ExpandKeys: []ExpandKey{{Key: "status", Type: "STRING"}}},
			wantErr: []string{`expand_keys[0]: type "STRING" is not one of TEXT, INTEGER, DOUBLE, BOOLEAN`},
		},
		{
			name:   "expand_keys valid",
			schema: Schema{ExpandKeys: []ExpandKey{{Key: "status", Type: "INTEGER"}}},
		},
		{
			name:    "none and empty are always valid field refs",
			schema:  Schema{TimestampField: "none", LevelField: "", TraceIDField: "none"},
			wantErr: nil,
		},
		{
			name:   "multiple problems all reported at once",
			schema: Schema{Format: "bogus", Envelope: "bogus", TimestampFields: -5},
			wantErr: []string{
				`format: "bogus" is not one of text, json, prefix_json`,
				`envelope: "bogus" is not one of none, kevcons`,
				"timestamp_fields: must be >= 0",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.schema.Validate()
			if len(tc.wantErr) == 0 {
				if len(got) != 0 {
					t.Fatalf("expected no errors, got %v", got)
				}
				return
			}
			if len(got) != len(tc.wantErr) {
				t.Fatalf("expected %d errors, got %d: %v", len(tc.wantErr), len(got), got)
			}
			for i, want := range tc.wantErr {
				if !contains(got[i], want) {
					t.Errorf("error[%d] = %q, want substring %q", i, got[i], want)
				}
			}
		})
	}
}

const time_RFC3339 = "2006-01-02T15:04:05Z07:00"

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
