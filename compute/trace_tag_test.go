package compute

import (
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestLocalComputeJob_TraceTagEmitsSummaryFirst: a bare interactive
// `trace <id>` (traceTag=true, no compute stages) buffers the full
// merged stream and emits a synthetic summary line — mentioning the
// line and distinct-service counts — before any log row.
func TestLocalComputeJob_TraceTagEmitsSummaryFirst(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 2, "ts", 0, false, true).(*LocalComputeJob)

	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: `{"msg":"a"}`, MetaVars: map[string]string{"namespace": "acme", "service": "api"}}, "a")
	job.SendSentinel("a")
	job.SendRow(&pb.ResultRow{TsUnixNs: 2000, Raw: `{"msg":"b"}`, MetaVars: map[string]string{"namespace": "acme", "service": "edge"}}, "b")
	job.SendSentinel("b")

	lines, err := DrainLineStream(job.Stream())
	if err != nil {
		t.Fatalf("DrainLineStream: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (1 summary + 2 rows): %v", len(lines), lines)
	}
	summary := lines[0].Raw
	if !strings.Contains(summary, "2 line(s)") || !strings.Contains(summary, "2 service(s)") {
		t.Errorf("summary line = %q, want it to mention 2 line(s) and 2 service(s)", summary)
	}
}

// TestLocalComputeJob_TraceTagPrefixesEachRowWithBadge: every row
// after the summary carries a [namespace/service] badge built from
// its MetaVars.
func TestLocalComputeJob_TraceTagPrefixesEachRowWithBadge(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 1, "ts", 0, false, true).(*LocalComputeJob)

	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "hello", MetaVars: map[string]string{"namespace": "acme", "service": "api"}}, "a")
	job.SendSentinel("a")

	lines, err := DrainLineStream(job.Stream())
	if err != nil {
		t.Fatalf("DrainLineStream: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (1 summary + 1 row): %v", len(lines), lines)
	}
	row := lines[1].Raw
	if !strings.Contains(row, "[acme/api]") {
		t.Errorf("row = %q, want it to contain badge \"[acme/api]\"", row)
	}
	if !strings.HasSuffix(row, "hello") {
		t.Errorf("row = %q, want it to end with the original raw text %q", row, "hello")
	}
}

// TestLocalComputeJob_TraceTagZeroRows: a trace ID with no matching
// rows gets a distinct "no lines found" message rather than a summary
// claiming "0 line(s) across 0 service(s)".
func TestLocalComputeJob_TraceTagZeroRows(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 1, "ts", 0, false, true).(*LocalComputeJob)
	job.SendSentinel("a")

	lines, err := DrainLineStream(job.Stream())
	if err != nil {
		t.Fatalf("DrainLineStream: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0].Raw, "no lines found") {
		t.Errorf("empty-result line = %q, want it to contain %q", lines[0].Raw, "no lines found")
	}
}

// TestLocalComputeJob_TraceTagRespectsLimit: the buffering branch
// still honors j.limit as a hard cap via the onlineMerge emit
// callback, same mechanism the normal zero-stage branch uses.
func TestLocalComputeJob_TraceTagRespectsLimit(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 1, "ts", 2, false, true).(*LocalComputeJob)

	for _, r := range []*pb.ResultRow{
		{TsUnixNs: 1000, Raw: "one", MetaVars: map[string]string{"namespace": "acme", "service": "api"}},
		{TsUnixNs: 2000, Raw: "two", MetaVars: map[string]string{"namespace": "acme", "service": "api"}},
		{TsUnixNs: 3000, Raw: "three", MetaVars: map[string]string{"namespace": "acme", "service": "api"}},
	} {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")

	lines, err := DrainLineStream(job.Stream())
	if err != nil {
		t.Fatalf("DrainLineStream: %v", err)
	}
	// 1 summary + 2 rows (limit=2), never the third row.
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (1 summary + 2 capped rows): %v", len(lines), lines)
	}
	for _, l := range lines[1:] {
		if strings.Contains(l.Raw, "three") {
			t.Errorf("limit=2 but row %q leaked past the cap", l.Raw)
		}
	}
}

// TestLocalComputeJob_TraceTagEmptyNamespaceService: a row missing
// namespace/service metadata (a malformed or pre-metavar shard
// response) gets an "[unknown]" badge rather than a blank or
// malformed "[/]" tag.
func TestLocalComputeJob_TraceTagEmptyNamespaceService(t *testing.T) {
	pool := &LocalComputePool{}
	job := pool.NewJob(nil, 1, "ts", 0, false, true).(*LocalComputeJob)

	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "mystery"}, "a")
	job.SendSentinel("a")

	lines, err := DrainLineStream(job.Stream())
	if err != nil {
		t.Fatalf("DrainLineStream: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (1 summary + 1 row): %v", len(lines), lines)
	}
	if !strings.Contains(lines[1].Raw, "[unknown]") {
		t.Errorf("row = %q, want it to contain \"[unknown]\" badge", lines[1].Raw)
	}
}
