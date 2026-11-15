package ingest

import (
	"reflect"
	"testing"
	"time"
)

func TestCompileJSONPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []jsonPathSegment
		wantErr bool
	}{
		{
			name: "single leading-dot key — today's only supported shape",
			path: ".trace_id",
			want: []jsonPathSegment{{key: "trace_id", index: -1}},
		},
		{
			name: "key with no leading dot",
			path: "trace_id",
			want: []jsonPathSegment{{key: "trace_id", index: -1}},
		},
		{
			name: "nested key",
			path: ".context.trace_id",
			want: []jsonPathSegment{{key: "context", index: -1}, {key: "trace_id", index: -1}},
		},
		{
			name: "array index then key",
			path: ".tags[0].value",
			want: []jsonPathSegment{{key: "tags", index: -1}, {index: 0}, {key: "value", index: -1}},
		},
		{
			name: "chained indices",
			path: ".items[2][0]",
			want: []jsonPathSegment{{key: "items", index: -1}, {index: 2}, {index: 0}},
		},
		{name: "empty path", path: "", wantErr: true},
		{name: "just a dot", path: ".", wantErr: true},
		{name: "empty segment", path: ".a..b", wantErr: true},
		{name: "unterminated bracket", path: ".tags[0", wantErr: true},
		{name: "non-numeric index", path: ".tags[x]", wantErr: true},
		{name: "negative index", path: ".tags[-1]", wantErr: true},
		{name: "filter expression is unsupported, not silently mismatched", path: ".tags[?(@.key==\"x\")]", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compileJSONPath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("compileJSONPath(%q) = %v, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("compileJSONPath(%q) unexpected error: %v", tc.path, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("compileJSONPath(%q) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
}

func TestEvalJSONPath(t *testing.T) {
	var doc any
	body := `{
		"trace_id": "abc123",
		"context": {"trace": {"id": "nested-id"}},
		"tags": [{"key": "env", "value": "prod"}, {"key": "trace_id", "value": "array-id"}],
		"count": 3
	}`
	if _, ok := parseJSONBody(body); !ok {
		t.Fatal("parseJSONBody failed to parse test fixture")
	}
	doc, _ = parseJSONBody(body)

	tests := []struct {
		name string
		path string
		want any
		ok   bool
	}{
		{name: "flat key — today's only shape, must keep working", path: ".trace_id", want: "abc123", ok: true},
		{name: "nested object", path: ".context.trace.id", want: "nested-id", ok: true},
		{name: "array index then key", path: ".tags[1].value", want: "array-id", ok: true},
		{name: "missing key", path: ".nope", ok: false},
		{name: "index out of range", path: ".tags[5].value", ok: false},
		{name: "indexing into a non-array", path: ".trace_id[0]", ok: false},
		{name: "keying into a non-object", path: ".count.nope", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			segs, err := compileJSONPath(tc.path)
			if err != nil {
				t.Fatalf("compileJSONPath(%q): %v", tc.path, err)
			}
			got, ok := evalJSONPath(doc, segs)
			if ok != tc.ok {
				t.Fatalf("evalJSONPath(%q) ok = %v, want %v (got %v)", tc.path, ok, tc.ok, got)
			}
			if tc.ok && got != tc.want {
				t.Errorf("evalJSONPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestParseLine_NestedJSONPaths is the end-to-end case the flat obj[key]
// lookup could never reach: every tag-style field plus timestamp pulled
// from a nested/array JSON structure in one row, through the real
// NewParser/ParseLine path (not just compileJSONPath/evalJSONPath in
// isolation).
func TestParseLine_NestedJSONPaths(t *testing.T) {
	schema := Schema{
		Format:          "json",
		TimestampField:  ".context.ts",
		TimestampFormat: time.RFC3339,
		LevelField:      ".context.level",
		TraceIDField:    ".context.trace.id",
		SpanNameField:   ".tags[0].value",
		SpanIDField:     ".tags[1].value",
	}
	parser, err := NewParser(&schema)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}

	line := `{"context":{"ts":"2026-09-24T10:15:03Z","level":"info","trace":{"id":"nested-trace-1"}},"tags":[{"key":"span","value":"handler.run"},{"key":"span_id","value":"span-42"}]}`
	row, err := parser.ParseLine(line, time.Now())
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}

	wantTs := time.Date(2026, 9, 24, 10, 15, 3, 0, time.UTC).UnixNano()
	if row.TsUnixNs != wantTs {
		t.Errorf("TsUnixNs = %d, want %d", row.TsUnixNs, wantTs)
	}
	if row.Level != "INFO" {
		t.Errorf("Level = %q, want INFO", row.Level)
	}
	if row.TraceId != "nested-trace-1" {
		t.Errorf("TraceId = %q, want nested-trace-1", row.TraceId)
	}
	if row.SpanName != "handler.run" {
		t.Errorf("SpanName = %q, want handler.run", row.SpanName)
	}
	if row.SpanId != "span-42" {
		t.Errorf("SpanId = %q, want span-42", row.SpanId)
	}
}

// TestNewParser_RejectsUnsupportedJSONPath ensures a schema with a path
// compileJSONPath can't handle fails fast at parser construction — the
// same convention NewParser already uses for a bad regexp pattern —
// rather than silently misbehaving per row.
func TestNewParser_RejectsUnsupportedJSONPath(t *testing.T) {
	schema := Schema{
		Format:       "json",
		TraceIDField: ".tags[?(@.key==\"x\")]",
	}
	if _, err := NewParser(&schema); err == nil {
		t.Fatal("NewParser accepted an unsupported JSON path, want an error")
	}
}

// BenchmarkParseLine_JSONManyFields is the "up to 7 redundant
// json.Unmarshal calls per row" hot path this plan's Performance
// implications section calls out — every tag-style field JSON-configured
// on one schema, run through the real per-row entry point.
func BenchmarkParseLine_JSONManyFields(b *testing.B) {
	schema := Schema{
		Format:            "json",
		TimestampField:    ".ts",
		TimestampFormat:   time.RFC3339,
		LevelField:        ".level",
		TraceIDField:      ".trace_id",
		SpanNameField:     ".span_name",
		SpanIDField:       ".span_id",
		ParentSpanIDField: ".parent_span_id",
		DurationNsField:   ".duration_ns",
	}
	parser, err := NewParser(&schema)
	if err != nil {
		b.Fatalf("NewParser: %v", err)
	}
	line := `{"ts":"2026-09-24T10:15:03Z","level":"info","trace_id":"t1","span_name":"handler","span_id":"s1","parent_span_id":"p1","duration_ns":"1500000"}`
	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parser.ParseLine(line, now); err != nil {
			b.Fatal(err)
		}
	}
}
