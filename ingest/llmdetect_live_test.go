package ingest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Live Bedrock tests: opt-in, because they spend a few model calls and need
// AWS credentials. Run with, for example,
//
//	BEDROCK_LIVE_MODEL_ID=us.amazon.nova-lite-v1:0 BEDROCK_LIVE_REGION=us-east-2 \
//	  go test -tags test ./ingest -run Live -v
func liveClassifier(t *testing.T) *BedrockClassifier {
	t.Helper()
	model, region := os.Getenv("BEDROCK_LIVE_MODEL_ID"), os.Getenv("BEDROCK_LIVE_REGION")
	if model == "" || region == "" {
		t.Skip("set BEDROCK_LIVE_MODEL_ID and BEDROCK_LIVE_REGION to run live Bedrock tests")
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	return NewBedrockClassifier(cfg, model)
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
	det := Detect(lines)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	raw, vlines, err := c.classifyRaw(ctx, det)
	if err != nil {
		t.Fatalf("classifyRaw: %v", err)
	}
	t.Logf("model proposed: timestamp=%+v level=%+v trace=%+v", raw.Timestamp, raw.Level, raw.TraceID)
	res := validateClassification(raw, vlines)
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
	marks := []ProbeMark{
		{Line: 0, Prefix: "2026-10-15 12:00:01 handled id=", Text: "ab12"},
		{Line: 1, Prefix: "2026-10-15 12:00:02 handled ID: ", Text: "cd34"},
		{Line: 2, None: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rawIn := ProposeInput{Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines, Examples: marks[:2], NoValueLines: []int{2}}
	if raw, err := c.proposeRaw(ctx, rawIn); err != nil {
		t.Fatalf("proposeRaw: %v", err)
	} else {
		t.Logf("model proposed: %+v", raw.Candidates)
	}
	res, err := ProbeContext(ctx, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines, Marks: marks, Proposer: c,
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

// TestLiveRawToolInput prints what the model actually returns for the
// classifier tool, before any decoding: the first thing to look at when
// proposals come back empty.
func TestLiveRawToolInput(t *testing.T) {
	c := liveClassifier(t)
	lines := []string{
		"2026-10-15 12:00:00.000 INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a871",
		"2026-10-15 12:00:01.000 DEBUG handled GET /healthz 200 1ms",
		"2026-10-15 12:00:02.000 INFO handled GET /api/orders 500 90ms trace_id=aa01bb02cc03dd04",
		"2026-10-15 12:00:03.000 DEBUG handled GET /healthz 200 1ms",
	}
	det := Detect(lines)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.client.Converse(ctx, classifyConverseInput(c.modelID, det, lines))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := extractToolUseInput(resp)
	if err != nil {
		t.Fatal(err)
	}
	b, err := raw.MarshalSmithyDocument()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("raw tool input: %s", b)
}
