package main

import "testing"

// TestBuildLogRow_StructuralTraceIDOverridesExtraction: a socket-envelope
// (structural) trace ID is higher-fidelity than schema-based
// trace_id_field text extraction and must win when both are present.
func TestBuildLogRow_StructuralTraceIDOverridesExtraction(t *testing.T) {
	app := "traceidtest/structural"
	schemaYAML := "format: text\ntrace_id_field: \"2\"\n"
	if _, err := parserCache.Install(app, "kevlar", schemaYAML); err != nil {
		t.Fatalf("Install: %v", err)
	}

	row := buildLogRow("prefix text-extracted-id rest", "kevlar", "i-1", app, "structural-id")
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
	if _, err := parserCache.Install(app, "kevlar", schemaYAML); err != nil {
		t.Fatalf("Install: %v", err)
	}

	row := buildLogRow("prefix text-extracted-id rest", "kevlar", "i-1", app, "")
	if row.TraceId != "text-extracted-id" {
		t.Errorf("TraceId: got %q, want schema-extracted %q", row.TraceId, "text-extracted-id")
	}
}

// TestBuildLogRow_FallbackBranchCarriesTraceID covers the no-parser
// fallback branch (parseClientSide off, or no schema installed for the
// app): traceID must pass straight through onto the constructed LogRow.
func TestBuildLogRow_FallbackBranchCarriesTraceID(t *testing.T) {
	row := buildLogRow("no schema installed for this app", "kevlar", "i-1", "traceidtest/fallback", "fallback-id")
	if row.TraceId != "fallback-id" {
		t.Errorf("TraceId: got %q, want %q", row.TraceId, "fallback-id")
	}
}
