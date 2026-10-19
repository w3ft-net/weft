package main

import "testing"

// TestBuildLogRow_StructuralTraceIDOverridesExtraction: a socket-envelope
// (structural) trace ID is higher-fidelity than schema-based
// trace_id_field text extraction and must win when both are present.
func TestBuildLogRow_StructuralTraceIDOverridesExtraction(t *testing.T) {
	app := "traceidtest/structural"
	schemaYAML := "format: text\ntrace_id_field: \"2\"\n"
	if _, err := parserCache.Install(app, schemaYAML); err != nil {
		t.Fatalf("Install: %v", err)
	}

	row := buildLogRow("prefix text-extracted-id rest", "i-1", app, "structural-id")
	if row.TraceId != "structural-id" {
		t.Errorf("TraceId: got %q, want structural override %q", row.TraceId, "structural-id")
	}
}

// TestBuildLogRow_NoOverrideKeepsExtraction is the regression guard for
// the property that ParseLine's schema-based extraction survives
// buildLogRow's post-parse field overwrites untouched when there's no
// structural (socket-envelope) trace ID to override it with.
func TestBuildLogRow_NoOverrideKeepsExtraction(t *testing.T) {
	app := "traceidtest/textonly"
	schemaYAML := "format: text\ntrace_id_field: \"2\"\n"
	if _, err := parserCache.Install(app, schemaYAML); err != nil {
		t.Fatalf("Install: %v", err)
	}

	row := buildLogRow("prefix text-extracted-id rest", "i-1", app, "")
	if row.TraceId != "text-extracted-id" {
		t.Errorf("TraceId: got %q, want schema-extracted %q", row.TraceId, "text-extracted-id")
	}
}

// TestBuildLogRow_FallbackBranchCarriesTraceID covers the no-parser
// fallback branch (parseClientSide off, or no schema installed for the
// app): traceID must pass straight through onto the constructed LogRow.
func TestBuildLogRow_FallbackBranchCarriesTraceID(t *testing.T) {
	row := buildLogRow("no schema installed for this app", "i-1", "traceidtest/fallback", "fallback-id")
	if row.TraceId != "fallback-id" {
		t.Errorf("TraceId: got %q, want %q", row.TraceId, "fallback-id")
	}
}

// An SDK header fills the row's own fields and is stripped from raw, with
// no schema installed for the app.
func TestBuildLogRow_HeaderFillsFields(t *testing.T) {
	line := "\x1ev1\x1f1700000000000000042\x1fwarn\x1ftrace-1\x1fdb.query\x1fspan-1\x1fspan-0\x1f2500\x1fjson\x1fstderr\x1e" + `{"msg":"slow"}`
	row := buildLogRow(line, "i-1", "hdrtest/svc/main", "")

	if row.Raw != `{"msg":"slow"}` {
		t.Errorf("Raw = %q, want the body only", row.Raw)
	}
	if row.TsUnixNs != 1700000000000000042 || row.Level != "WARN" || row.TraceId != "trace-1" {
		t.Errorf("ts=%d level=%q trace=%q", row.TsUnixNs, row.Level, row.TraceId)
	}
	if row.SpanName != "db.query" || row.SpanId != "span-1" || row.ParentSpanId != "span-0" || row.DurationNs != 2500 {
		t.Errorf("span fields = %q %q %q %d", row.SpanName, row.SpanId, row.ParentSpanId, row.DurationNs)
	}
	if row.Kind != "stderr" || row.Service != "svc" || row.Namespace != "hdrtest" {
		t.Errorf("namespace/service/kind = %q/%q/%q, want hdrtest/svc/stderr", row.Namespace, row.Service, row.Kind)
	}
	if string(row.PayloadJson) != `{"msg":"slow"}` {
		t.Errorf("PayloadJson = %q, want the JSON body", row.PayloadJson)
	}
}

// A header value wins over schema extraction; a value it leaves empty keeps
// what the parser found.
func TestBuildLogRow_HeaderOverridesParserOnlyWhereSupplied(t *testing.T) {
	app := "hdrtest/override/main"
	if _, err := parserCache.Install(app, "format: text\ntrace_id_field: \"2\"\nlevel_field: \"1\"\n"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// The header supplies a trace ID but no level.
	row := buildLogRow("\x1ev1\x1f\x1f\x1fheader-trace\x1e"+"error extracted-trace rest", "i-1", app, "")
	if row.TraceId != "header-trace" {
		t.Errorf("TraceId = %q, want the header's", row.TraceId)
	}
	if row.Level != "ERROR" {
		t.Errorf("Level = %q, want the parser's, since the header left it empty", row.Level)
	}
}

// A line with no header is untouched, and fmt=text sets no payload.
func TestBuildLogRow_NoHeaderAndTextFmt(t *testing.T) {
	row := buildLogRow("just a line", "i-1", "hdrtest/plain/main", "")
	if row.Raw != "just a line" || row.Kind != "main" || row.Level != "" {
		t.Errorf("plain line changed: raw=%q kind=%q level=%q", row.Raw, row.Kind, row.Level)
	}
	row = buildLogRow("\x1ev1\x1f\x1f\x1f\x1f\x1f\x1f\x1f\x1f"+"text\x1e"+"hello", "i-1", "hdrtest/plain/main", "")
	if row.Raw != "hello" || len(row.PayloadJson) != 0 {
		t.Errorf("raw=%q payload=%q, want the body and no payload", row.Raw, row.PayloadJson)
	}
}

// A body that is not valid JSON falls back to raw only even when fmt=json.
func TestBuildLogRow_InvalidJSONBodyFallsBack(t *testing.T) {
	row := buildLogRow("\x1ev1\x1f\x1f\x1f\x1f\x1f\x1f\x1f\x1f"+"json\x1e"+"{not json", "i-1", "hdrtest/badjson/main", "")
	if row.Raw != "{not json" || len(row.PayloadJson) != 0 {
		t.Errorf("raw=%q payload=%q, want raw only", row.Raw, row.PayloadJson)
	}
}

// The legacy socket-style header still yields just a trace ID.
func TestBuildLogRow_LegacyHeaderInFile(t *testing.T) {
	row := buildLogRow("\x1eabc\x1eold style", "i-1", "hdrtest/legacy/main", "")
	if row.TraceId != "abc" || row.Raw != "old style" {
		t.Errorf("trace=%q raw=%q", row.TraceId, row.Raw)
	}
}
