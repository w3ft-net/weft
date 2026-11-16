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
		{
			// The shape weft's own logging uses everywhere (logQuery,
			// cold_shard.go, lambda-shard's handler): a trailing
			// "trace_id=<id>" key=value tag, not a bare token.
			name: "trace_id_pattern strips the key= prefix",
			schema: Schema{
				Format:         "text",
				TraceIDField:   "11",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    "2026/09/23 22:24:09 lambda-shard: query completed in 7.9s parquet=s3://bucket/x.parquet (3 rows) trace_id=04820d2e6642041168fb17ed0a55f9c6",
			wantTID: "04820d2e6642041168fb17ed0a55f9c6",
		},
		{
			name: "trace_id_pattern non-match yields empty, not the raw field",
			schema: Schema{
				Format:         "text",
				TraceIDField:   "3",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    "2026-01-01 INFO no-key-value-tag-here",
			wantTID: "",
		},
		{
			// TraceIDPattern only applies on the positional-field path —
			// same relationship LevelPattern has to the JSON dot-path
			// (extractJSONLevel never applies levelRe either).
			name: "trace_id_pattern is ignored on the JSON dot-path",
			schema: Schema{
				Format:         "json",
				TraceIDField:   ".trace_id",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    `{"trace_id":"trace-xyz-789"}`,
			wantTID: "trace-xyz-789",
		},
		{
			// coordinator/querylog.go's real shape: the SQL text before
			// trace_id= varies in length line to line (different WHERE
			// clauses, LIMITs, predicates), so no single TraceIDField
			// index could ever land on it — only a whole-line search
			// works here. TraceIDField left empty/unset is what selects
			// this mode.
			name: "empty trace_id_field searches the whole line when trace_id_pattern is set",
			schema: Schema{
				Format:         "text",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    `2026/09/23 17:16:07 query: namespace=weft service=coordinator shards=[shard-a shard-b] compute=[] sql=SELECT raw, ts FROM logs WHERE customer = ? AND ts >= CAST(? AS TIMESTAMP) ORDER BY ts LIMIT 5 trace_id=20a6a413d1a64330914e577274620763 coordinator_id=i-003aaf778951a6907`,
			wantTID: "20a6a413d1a64330914e577274620763",
		},
		{
			// api/middleware.go's real shape: method+path+status+duration
			// all vary in field count/width before trace_id=.
			name: "whole-line search on api's access-log shape",
			schema: Schema{
				Format:         "text",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    `access GET /api/sources/weft/cold-shard/schema 200 12ms user=abc123 trace_id=f991d26350ece3d0c1de13d5817d24ad`,
			wantTID: "f991d26350ece3d0c1de13d5817d24ad",
		},
		{
			// A line with genuinely no tag must stay empty, not fall back
			// to the JSON-timestamp trick of scanning for a stray brace
			// or anything else — the whole-line search only ever finds
			// what the pattern actually matches.
			name: "whole-line search finds nothing on an untagged line",
			schema: Schema{
				Format:         "text",
				TraceIDPattern: `trace_id=(\S+)`,
			},
			line:    "2026/09/23 17:16:07 cron[mem-pressure]: ok (19ms, stdout=0 bytes, stderr=0 bytes)",
			wantTID: "",
		},
		{
			// No pattern and no field at all: still fully disabled,
			// unchanged from before this mode existed.
			name: "no trace_id_field and no trace_id_pattern means no extraction",
			schema: Schema{
				Format: "text",
			},
			line:    "2026/09/23 17:16:07 query: ... trace_id=should-not-be-found",
			wantTID: "",
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

// TestExtractSpanFields covers span_name/span_id/parent_span_id/duration_ns
// extraction against weft-go's real "span done" line shape — a whole-line
// pattern search (no fixed field index works, since the fields preceding
// each key=value tag vary in count), same convention TestExtractTraceID's
// "whole-line search" cases establish for trace_id_pattern.
func TestExtractSpanFields(t *testing.T) {
	schema := Schema{
		Format:              "text",
		SpanNamePattern:     `name=(\S+)`,
		SpanIDPattern:       `span_id=(\S+)`,
		ParentSpanIDPattern: `parent_span_id=(\S*)`,
		DurationNsPattern:   `duration_ns=(\d+)`,
	}
	parser, err := NewParser(&schema)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	t.Run("span done line: all four fields", func(t *testing.T) {
		line := "span done name=shard.Query trace_id=abc span_id=s2 parent_span_id=s1 duration=1.234ms duration_ns=1234000 status=ok"
		row, err := parser.ParseLine(line, time.Now())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if row.SpanName != "shard.Query" {
			t.Errorf("SpanName: got %q, want shard.Query", row.SpanName)
		}
		if row.SpanId != "s2" {
			t.Errorf("SpanId: got %q, want s2", row.SpanId)
		}
		if row.ParentSpanId != "s1" {
			t.Errorf("ParentSpanId: got %q, want s1", row.ParentSpanId)
		}
		if row.DurationNs != 1234000 {
			t.Errorf("DurationNs: got %d, want 1234000", row.DurationNs)
		}
	})

	t.Run("span start line: no duration_ns tag at all", func(t *testing.T) {
		line := "span start name=coordinator.Execute trace_id=abc span_id=s1 parent_span_id="
		row, err := parser.ParseLine(line, time.Now())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if row.SpanName != "coordinator.Execute" {
			t.Errorf("SpanName: got %q, want coordinator.Execute", row.SpanName)
		}
		if row.DurationNs != 0 {
			t.Errorf("DurationNs: got %d, want 0 (no tag on a span start line)", row.DurationNs)
		}
	})

	t.Run("empty parent_span_id parses as empty string, not an error", func(t *testing.T) {
		line := "span done name=api.access trace_id=abc span_id=s1 parent_span_id= duration=1ms duration_ns=1000000 status=ok"
		row, err := parser.ParseLine(line, time.Now())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if row.ParentSpanId != "" {
			t.Errorf("ParentSpanId: got %q, want empty", row.ParentSpanId)
		}
	})

	t.Run("non-span line: nothing matches", func(t *testing.T) {
		line := "2026/09/23 17:16:07 cron[mem-pressure]: ok (19ms)"
		row, err := parser.ParseLine(line, time.Now())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if row.SpanName != "" || row.SpanId != "" || row.ParentSpanId != "" || row.DurationNs != 0 {
			t.Errorf("expected all span fields empty, got %+v", row)
		}
	})
}
