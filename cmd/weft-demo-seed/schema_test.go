package main

import (
	"strings"
	"testing"
)

// TestMinimalSchemaYAML_TraceIDFieldGated pins the exact bug this gate
// exists to prevent: trace_id_field must only ever be set for a
// producer that actually emits a bare trace ID at field 3
// (cmd/loom-producer's Go binary, prod-only). Every dev-seeded
// customer — Loom included — still runs the unmodified Python
// dev/demo/generator.py, which doesn't; pointing trace_id_field at
// field 3 there would silently extract the event name into the
// reserved trace_id column instead.
func TestMinimalSchemaYAML_TraceIDFieldGated(t *testing.T) {
	withTrace := minimalSchemaYAML("api", true)
	if !strings.Contains(withTrace, `trace_id_field: "3"`) {
		t.Errorf("withTraceID=true schema missing trace_id_field: %s", withTrace)
	}

	withoutTrace := minimalSchemaYAML("api", false)
	if strings.Contains(withoutTrace, "trace_id_field") {
		t.Errorf("withTraceID=false schema unexpectedly has trace_id_field: %s", withoutTrace)
	}
}

func TestMinimalSchemaYAML_UnaffectedFieldsUnchanged(t *testing.T) {
	for _, withTrace := range []bool{true, false} {
		got := minimalSchemaYAML("worker", withTrace)
		for _, want := range []string{`timestamp_field: "1"`, `level_field: "2"`, "app: worker"} {
			if !strings.Contains(got, want) {
				t.Errorf("minimalSchemaYAML(%q, %v) missing %q: %s", "worker", withTrace, want, got)
			}
		}
	}
}
