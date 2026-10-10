package main

import (
	"strings"
	"testing"
)

// TestMinimalSchemaYAML_TraceIDFieldGated pins the bug this gate exists to
// prevent: trace_id_field must only be set for a producer that emits a bare
// trace ID at field 3 (cmd/loom-producer's Go binary). The Python
// dev/demo/generator.py behind acme and globex doesn't; pointing
// trace_id_field at its field 3 would silently extract the event name into
// the reserved trace_id column instead. See emitsTraceID for who qualifies.
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
