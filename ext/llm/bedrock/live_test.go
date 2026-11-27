package bedrock

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/w3ft-net/weft/ingest"
)

// Live Bedrock tests: opt-in, because they spend a few model calls and need
// AWS credentials. Run with, for example,
//
//	BEDROCK_LIVE_MODEL_ID=us.amazon.nova-lite-v1:0 BEDROCK_LIVE_REGION=us-east-2 \
//	  go test -tags test ./ingest -run Live -v
func liveClassifier(t *testing.T) ingest.LLMClassifier {
	t.Helper()
	model, region := os.Getenv("BEDROCK_LIVE_MODEL_ID"), os.Getenv("BEDROCK_LIVE_REGION")
	if model == "" || region == "" {
		t.Skip("set BEDROCK_LIVE_MODEL_ID and BEDROCK_LIVE_REGION to run live Bedrock tests")
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	return ingest.LLMClassifier{LLM: New(cfg, model)}
}

// Detection at sample time: the model's real proposals must survive the
// parser-backed verification for a sample where the trace id is on only some
// lines.
func TestLiveClassifyMinorityTraceID(t *testing.T) {
	c := liveClassifier(t)
	var lines []string
	for i := 0; i < 30; i++ {
		ts := time.Date(2026, 10, 15, 12, 0, i, 0, time.UTC).Format("2006-01-02 15:04:05.000")
		if i%3 == 0 {
			lines = append(lines, ts+" INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a8"+string(rune('0'+i%10))+"1")
		} else {
			lines = append(lines, ts+" DEBUG handled GET /healthz 200 1ms")
		}
	}
	det := ingest.Detect(lines)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.Classify(ctx, det)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	t.Logf("timestamp=%+v level=%+v trace=%+v", res.Timestamp, res.Level, res.TraceID)
	if res.TraceID == nil {
		t.Fatal("the model's trace id proposal did not survive verification (on 10 of 30 lines)")
	}
	if !strings.Contains(res.TraceID.Pattern, "trace_id") {
		t.Errorf("trace pattern = %q", res.TraceID.Pattern)
	}
}

// Retry: two spellings no simple rule covers; the real model is asked, and
// whatever it proposes is verified by the probe before it is accepted.
func TestLiveRetryProposesExtractorForTwoSpellings(t *testing.T) {
	c := liveClassifier(t)
	lines := []string{
		"2026-10-15 12:00:01 handled id=ab12 ok",
		"2026-10-15 12:00:02 handled ID: cd34 ok",
		"2026-10-15 12:00:03 handled nothing here",
		"2026-10-15 12:00:04 handled id=ef56 ok",
		"2026-10-15 12:00:05 handled ID: 0a1b ok",
	}
	marks := []ingest.ProbeMark{
		{Line: 0, Prefix: "2026-10-15 12:00:01 handled id=", Text: "ab12"},
		{Line: 1, Prefix: "2026-10-15 12:00:02 handled ID: ", Text: "cd34"},
		{Line: 2, None: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := ingest.ProbeContext(ctx, ingest.ProbeRequest{
		Field: ingest.ProbeTraceID, Base: ingest.ProbeBase{Format: "text"}, Lines: lines, Marks: marks, Proposer: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("extractor=%+v source=%q message=%q", res.Extractor, res.Source, res.Message)
	if res.Extractor == nil {
		t.Fatalf("the model proposed nothing the probe could verify: %s", res.Message)
	}
	if res.Source != "model" {
		t.Errorf("source = %q, want model", res.Source)
	}
	want := map[int]string{0: "ab12", 1: "cd34", 2: "", 3: "ef56", 4: "0a1b"}
	for _, r := range res.Results {
		if r.Value != want[r.Line] {
			t.Errorf("line %d: %q, want %q", r.Line, r.Value, want[r.Line])
		}
	}
}

// A span-emitting service interleaves start/done pairs (duration only on
// "done"), error lines and their continuation lines. Every span field must
// survive verification even though none is on every line.
func TestLiveClassifySpanStartDoneSample(t *testing.T) {
	c := liveClassifier(t)
	var lines []string
	for i := 0; i < 10; i++ {
		ts := time.Date(2026, 10, 16, 9, 42, i*3, 0, time.UTC).Format("2006/01/02 15:04:05")
		tid, sid, pid := fmt.Sprintf("%032x", i*7919+1), fmt.Sprintf("%016x", i*104729+3), fmt.Sprintf("%016x", i*1299709+5)
		lines = append(lines,
			ts+" span start name=compute.ExecuteQueryStream trace_id="+tid+" span_id="+sid+" parent_span_id="+pid,
			ts+" cold shard shard-20260831T0000-7f25c473: lambda returned 500: query failed: IO Error: No files found",
			"LINE 1: ... raw, ts FROM read_parquet('s3://weft-cold/x')",
			"^",
			ts+" span done name=compute.ExecuteQueryStream trace_id="+tid+" span_id="+sid+" parent_span_id="+pid+
				fmt.Sprintf(" duration=%d.%09ds duration_ns=%d status=ok", 1+i%4, 1234567+i, 1001234567+i),
		)
	}
	det := ingest.Detect(lines)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := c.Classify(ctx, det)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	t.Logf("verified: trace=%+v name=%+v span=%+v parent=%+v dur=%+v",
		res.TraceID, res.SpanName, res.SpanID, res.ParentSpanID, res.DurationNs)
	if res.DurationNs == nil {
		t.Error("no verified duration_ns candidate")
	}
}

// With nothing marked, the real model must propose a duration rule and a
// unit that the parser verifies on the sample.
func TestLiveProposeFirstDurationGuess(t *testing.T) {
	c := liveClassifier(t)
	var lines []string
	for i := 0; i < 8; i++ {
		ts := time.Date(2026, 10, 16, 9, 42, i*3, 0, time.UTC).Format("2006/01/02 15:04:05")
		lines = append(lines,
			ts+" span start name=compute.ExecuteQueryStream span_id="+fmt.Sprintf("%016x", i+1),
			ts+" cold shard shard-1: lambda returned 500: query failed",
			ts+" span done name=compute.ExecuteQueryStream span_id="+fmt.Sprintf("%016x", i+1)+fmt.Sprintf(" took=%d.%03ds status=ok", 1+i%3, 120+i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := ingest.ProbeContext(ctx, ingest.ProbeRequest{Field: ingest.ProbeDurationNs, Lines: lines, Propose: true, Proposer: c})
	if err != nil {
		t.Fatal(err)
	}
	if res.Extractor == nil {
		t.Fatalf("no first guess: %s", res.Message)
	}
	t.Logf("guess: %+v (%s), matched %d/%d", *res.Extractor, res.Extractor.Describe(), res.Matched, res.Total)
	for _, r := range res.Results {
		if r.Matched {
			t.Logf("line %d -> %s ns", r.Line, r.Value)
			break
		}
	}
	if res.Matched < 2 {
		t.Errorf("guess reads only %d lines", res.Matched)
	}
}

// With nothing marked, a first guess for an ordinary tag value (not a duration).
func TestLiveProposeFirstTraceGuess(t *testing.T) {
	c := liveClassifier(t)
	var lines []string
	for i := 0; i < 12; i++ {
		ts := time.Date(2026, 10, 16, 9, 42, i, 0, time.UTC).Format("2006-01-02 15:04:05.000")
		if i%3 == 0 {
			lines = append(lines, ts+" INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a8"+fmt.Sprintf("%02d", i))
		} else {
			lines = append(lines, ts+" DEBUG handled GET /healthz 200 1ms")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := ingest.ProbeContext(ctx, ingest.ProbeRequest{Field: ingest.ProbeTraceID, Base: ingest.ProbeBase{Format: "text"}, Lines: lines, Propose: true, Proposer: c})
	if err != nil {
		t.Fatal(err)
	}
	if res.Extractor == nil {
		t.Fatalf("no first guess: %s", res.Message)
	}
	t.Logf("guess: %s, matched %d/%d", res.Extractor.Describe(), res.Matched, res.Total)
	if res.Matched != 4 {
		t.Errorf("guess reads %d lines, want the 4 that carry a trace id", res.Matched)
	}
}
