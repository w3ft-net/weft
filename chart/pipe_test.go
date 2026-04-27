package chart

import (
	"strings"
	"testing"
	"time"
)

func TestParsePipeArgs_Defaults(t *testing.T) {
	got, msg := parsePipeArgs([]string{})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "bar" {
		t.Errorf("default Kind: got %q, want bar", got.Kind)
	}
	if got.Aggregate != "identity" {
		t.Errorf("default aggregate (no bucket): got %q, want identity", got.Aggregate)
	}
}

func TestParsePipeArgs_BucketImpliesLine(t *testing.T) {
	got, msg := parsePipeArgs([]string{"--bucket", "1m", "-x", ".ts", "-y", ".v"})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "line" {
		t.Errorf("bucket should default kind to line, got %q", got.Kind)
	}
	if got.Bucket != time.Minute {
		t.Errorf("Bucket: got %v, want 1m", got.Bucket)
	}
	if got.Aggregate != "count" {
		t.Errorf("bucket should default aggregate to count when y present, got %q", got.Aggregate)
	}
}

func TestParsePipeArgs_TypeOverride(t *testing.T) {
	got, msg := parsePipeArgs([]string{"--bucket", "1m", "-t", "bar"})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "bar" {
		t.Errorf("-t should override bucket default, got %q", got.Kind)
	}
}

func TestParsePipeArgs_RejectsMixedAddressing(t *testing.T) {
	_, msg := parsePipeArgs([]string{"-x", "1", "-y", ".latency"})
	if msg == "" {
		t.Fatal("expected error for mixed positional + JSON-path")
	}
	if !strings.Contains(msg, "must both be positional or both") {
		t.Errorf("error message: got %q", msg)
	}
}

func TestParsePipeArgs_AggregateValidation(t *testing.T) {
	_, msg := parsePipeArgs([]string{"-a", "median"})
	if msg == "" || !strings.Contains(msg, "unknown --aggregate") {
		t.Errorf("expected unknown aggregate error, got %q", msg)
	}
	for _, a := range []string{"count", "sum", "avg", "min", "max", "p50", "p95"} {
		_, msg := parsePipeArgs([]string{"-a", a})
		if msg != "" {
			t.Errorf("aggregate %q rejected unexpectedly: %s", a, msg)
		}
	}
}

func TestParsePipeArgs_RejectsPositional(t *testing.T) {
	_, msg := parsePipeArgs([]string{"my-watch"})
	if msg == "" {
		t.Fatal("expected error for stray positional arg in pipe mode")
	}
}

func TestRenderFromStream_DefaultBar(t *testing.T) {
	// `wc -l | chart` — one number per line, last whitespace field.
	args, msg := parsePipeArgs([]string{})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	out, errMsg := RenderFromStream([]string{"5", "12", "3"}, args, 60, 12)
	if errMsg != "" {
		t.Fatalf("render: %s", errMsg)
	}
	if out == "" {
		t.Fatal("empty output")
	}
}

func TestRenderFromStream_JSONLBucketedLine(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--bucket", "1m", "-a", "sum"})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`,
		`{"ts":"2026-01-01T00:00:30Z","v":2}`,
		`{"ts":"2026-01-01T00:01:05Z","v":4}`,
		`{"ts":"2026-01-01T00:01:50Z","v":8}`,
	}
	out, errMsg := RenderFromStream(lines, args, 60, 12)
	if errMsg != "" {
		t.Fatalf("render: %s", errMsg)
	}
	if out == "" {
		t.Fatal("empty output")
	}
}

func TestGroupRows_BucketedSum(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--bucket", "1m", "-a", "sum"})
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`,
		`{"ts":"2026-01-01T00:00:30Z","v":2}`,
		`{"ts":"2026-01-01T00:01:05Z","v":4}`,
		`{"ts":"2026-01-01T00:01:50Z","v":8}`,
	}
	groups, ordered, isTimeX, msg := groupRows(lines, args)
	if msg != "" {
		t.Fatalf("groupRows: %s", msg)
	}
	if !isTimeX {
		t.Error("expected isTimeX=true with --bucket")
	}
	if len(ordered) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(ordered))
	}
	// First Bucket: 1+2 = 3; second Bucket: 4+8 = 12.
	if reduce("sum", groups[ordered[0]]) != 3 {
		t.Errorf("bucket 0 sum: got %v, want 3", reduce("sum", groups[ordered[0]]))
	}
	if reduce("sum", groups[ordered[1]]) != 12 {
		t.Errorf("bucket 1 sum: got %v, want 12", reduce("sum", groups[ordered[1]]))
	}
}

func TestGroupRows_CategoricalBar(t *testing.T) {
	// `cat ... | jq '...' | chart -x .path -y .latency -a sum`
	args, _ := parsePipeArgs([]string{"-x", ".path", "-y", ".latency", "-a", "sum"})
	lines := []string{
		`{"path":"/api","latency":10}`,
		`{"path":"/healthz","latency":1}`,
		`{"path":"/api","latency":15}`,
		`{"path":"/api","latency":5}`,
	}
	groups, ordered, isTimeX, msg := groupRows(lines, args)
	if msg != "" {
		t.Fatalf("groupRows: %s", msg)
	}
	if isTimeX {
		t.Error("categorical x should not be time-x")
	}
	if len(ordered) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(ordered))
	}
	if ordered[0].label != "/api" {
		t.Errorf("first group: got %q, want /api", ordered[0].label)
	}
	if reduce("sum", groups[ordered[0]]) != 30 {
		t.Errorf("/api sum: got %v, want 30", reduce("sum", groups[ordered[0]]))
	}
	if reduce("sum", groups[ordered[1]]) != 1 {
		t.Errorf("/healthz sum: got %v, want 1", reduce("sum", groups[ordered[1]]))
	}
}

func TestReduce(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	cases := []struct {
		name string
		want float64
	}{
		{"count", 5},
		{"sum", 15},
		{"avg", 3},
		{"min", 1},
		{"max", 5},
		{"p50", 3},
		{"p95", 5},
		{"identity", 1},
	}
	for _, c := range cases {
		if got := reduce(c.name, v); got != c.want {
			t.Errorf("reduce(%q): got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseTime(t *testing.T) {
	if _, ok := parseTime("2026-01-01T00:00:00Z"); !ok {
		t.Error("RFC3339 not parsed")
	}
	if _, ok := parseTime("1736000000"); !ok {
		t.Error("epoch seconds not parsed")
	}
	if _, ok := parseTime("1736000000.5"); !ok {
		t.Error("epoch float not parsed")
	}
	if _, ok := parseTime("not a time"); ok {
		t.Error("string parsed as time unexpectedly")
	}
}
