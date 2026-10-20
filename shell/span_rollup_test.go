package shell

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecNonInteractive_SpansFlagRendersRollupTable(t *testing.T) {
	s := &Shell{
		Query: func(pipeline, rangeSpec string, dirStack []string) (*QueryResult, error) {
			return &QueryResult{Rows: []Row{
				{Raw: `{"name":"shard.Query","namespace":"weft","service":"coordinator","trace_id":"t1","span_id":"s1","parent_id":"","dur_ns":10000000,"ts_ns":1000,"level":"INFO"}`},
				{Raw: `{"name":"shard.Query","namespace":"weft","service":"shard-a","trace_id":"t1","span_id":"s2","parent_id":"s1","dur_ns":20000000,"ts_ns":2000,"level":"INFO"}`},
			}}, nil
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("trace t1 --spans", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "shard.Query") {
		t.Errorf("expected rollup table with span name, got:\n%s", out)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "COUNT") {
		t.Errorf("expected table header, got:\n%s", out)
	}
	if strings.Contains(out, "span done") {
		t.Errorf("expected rolled-up table, not raw span lines:\n%s", out)
	}
}

func TestExecNonInteractive_PlainTraceStillRendersGapHistogram(t *testing.T) {
	s := &Shell{
		Query: func(pipeline, rangeSpec string, dirStack []string) (*QueryResult, error) {
			return &QueryResult{Rows: []Row{
				{Raw: "1000\tweft/api\tsome log line"},
			}}, nil
		},
	}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("trace t1", &buf); err != nil {
		t.Fatalf("ExecNonInteractive: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "some log line") {
		t.Errorf("expected gap-histogram output with message text, got:\n%s", out)
	}
	if strings.Contains(out, "NAME") {
		t.Errorf("did not expect span rollup table for a plain trace query, got:\n%s", out)
	}
}

// TestExecNonInteractive_TraceSpansRendersRollupTable covers Phase 2's
// `trace spans [name]` non-interactive path — same renderer as Phase
// 1's --spans, different pipeline shape, cross-trace rows (distinct
// trace_id per occurrence).
func TestExecNonInteractive_TraceSpansRendersRollupTable(t *testing.T) {
	s := &Shell{
		Query: func(pipeline, rangeSpec string, dirStack []string) (*QueryResult, error) {
			return &QueryResult{Rows: []Row{
				{Raw: `{"name":"shard.Query","count":2,"errors":0,"total_ns":30000000,"avg_ns":15000000,"p50_ns":10000000,"p99_ns":20000000,"min_ns":10000000,"max_ns":20000000}`},
			}}, nil
		},
	}
	for _, pipeline := range []string{"trace spans", "trace spans shard.Query"} {
		t.Run(pipeline, func(t *testing.T) {
			var buf bytes.Buffer
			if err := s.ExecNonInteractive(pipeline, &buf); err != nil {
				t.Fatalf("ExecNonInteractive: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "shard.Query") {
				t.Errorf("expected rollup table with span name, got:\n%s", out)
			}
			if !strings.Contains(out, "NAME") || !strings.Contains(out, "COUNT") {
				t.Errorf("expected table header, got:\n%s", out)
			}
		})
	}
}

func TestIsSpansPipeline(t *testing.T) {
	cases := map[string]bool{
		"trace abc-123 --spans":    true,
		"trace --spans abc-123":    true,
		"trace spans":              true,
		"trace spans shard.Query":  true,
		"trace spans --by service": true,
		"trace spans | jq .name":   false,
		"trace spans a --samples":  false,
		"trace spans > /tmp/x":     false,
		"trace abc-123":            false,
		"cat warp.log --spans":     false,
		"":                         false,
	}
	for pipeline, want := range cases {
		if got := isSpansPipeline(pipeline); got != want {
			t.Errorf("isSpansPipeline(%q) = %v, want %v", pipeline, got, want)
		}
	}
}
