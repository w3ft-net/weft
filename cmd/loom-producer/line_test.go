package main

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// TestLineFor_TraceIDIsField3 pins the field layout ingest.Schema's
// trace_id_field (a fixed positive index, no negative/from-end form)
// depends on: field 1 = ts, field 2 = level, field 3 = the bare trace
// ID, regardless of service or event shape.
func TestLineFor_TraceIDIsField3(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, tc := range []struct {
		service, level string
	}{
		{"api", "INFO"}, {"api", "ERROR"},
		{"edge", "WARN"},
		{"worker", "INFO"}, {"worker", "ERROR"}, {"worker", "WARN"},
	} {
		line := LineFor(rng, tc.service, tc.level, "deadbeefdeadbeefdeadbeefdeadbeef")
		fields := strings.Fields(line)
		if len(fields) < 4 {
			t.Fatalf("%s/%s: line %q has fewer than 4 fields", tc.service, tc.level, line)
		}
		if fields[2] != "deadbeefdeadbeefdeadbeefdeadbeef" {
			t.Errorf("%s/%s: field 3 = %q, want the trace ID", tc.service, tc.level, fields[2])
		}
	}
}

func TestLineFor_TraceIDHasNoKeyPrefix(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	line := LineFor(rng, "api", "INFO", "abc123")
	if strings.Contains(line, "trace_id=") {
		t.Errorf("line %q should carry the trace ID as a bare token, not a trace_id= prefixed one (mirrors how LEVEL has no level= prefix)", line)
	}
}
