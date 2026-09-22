package ingest

import (
	"testing"
	"time"
)

// TestExtractTraceID covers trace_id_field extraction, mirroring
// LevelField's two shapes (1-based field index, JSON dot-path) minus the
// ToUpper call — trace IDs are case-sensitive opaque tokens, not enum-like
// levels.
func TestExtractTraceID(t *testing.T) {
	tests := []struct {
		name    string
		schema  Schema
		line    string
		wantTID string
	}{
		{
			name: "field index, text format",
			schema: Schema{
				Format:       "text",
				TraceIDField: "3",
			},
			line:    "2026-01-01 INFO trace-abc-123 request handled",
			wantTID: "trace-abc-123",
		},
		{
			name: "JSON dot-path",
			schema: Schema{
				Format:       "json",
				TraceIDField: ".trace_id",
			},
			line:    `{"trace_id":"trace-xyz-789","msg":"hi"}`,
			wantTID: "trace-xyz-789",
		},
		{
			name: "missing field is not an error",
			schema: Schema{
				Format:       "text",
				TraceIDField: "9",
			},
			line:    "2026-01-01 INFO short line",
			wantTID: "",
		},
		{
			name: "missing JSON key is not an error",
			schema: Schema{
				Format:       "json",
				TraceIDField: ".trace_id",
			},
			line:    `{"msg":"no trace here"}`,
			wantTID: "",
		},
		{
			name: "empty TraceIDField means no extraction",
			schema: Schema{
				Format: "text",
			},
			line:    "2026-01-01 INFO trace-abc-123 request handled",
			wantTID: "",
		},
		{
			name: "\"none\" means no extraction",
			schema: Schema{
				Format:       "text",
				TraceIDField: "none",
			},
			line:    "2026-01-01 INFO trace-abc-123 request handled",
			wantTID: "",
		},
		{
			name: "case preserved, unlike level",
			schema: Schema{
				Format:       "text",
				TraceIDField: "3",
			},
			line:    "2026-01-01 INFO Trace-Mixed-Case request handled",
			wantTID: "Trace-Mixed-Case",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parser, err := NewParser(&tc.schema)
			if err != nil {
				t.Fatalf("NewParser: %v", err)
			}
			row, err := parser.ParseLine(tc.line, time.Now())
			if err != nil {
				t.Fatalf("ParseLine: %v", err)
			}
			if row.TraceId != tc.wantTID {
				t.Errorf("TraceId: got %q, want %q", row.TraceId, tc.wantTID)
			}
		})
	}
}
