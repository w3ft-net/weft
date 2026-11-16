package ingest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStrftimeToGoLayout(t *testing.T) {
	tests := []struct {
		name          string
		pattern       string
		want          string
		wantErr       bool
		skipRoundTrip bool // for layouts whose literal text isn't a realistic timestamp (e.g. contains digits that collide with Go's reference-time tokens)
	}{
		{name: "ISO 8601", pattern: "%Y-%m-%dT%H:%M:%S%z", want: "2006-01-02T15:04:05-0700"},
		{name: "ISO 8601 no tz", pattern: "%Y-%m-%dT%H:%M:%S", want: "2006-01-02T15:04:05"},
		{name: "Python logging, millis via comma", pattern: "%Y-%m-%d %H:%M:%S,%3f", want: "2006-01-02 15:04:05,000"},
		{name: "microseconds", pattern: "%Y-%m-%d %H:%M:%S.%f", want: "2006-01-02 15:04:05.000000"},
		{name: "nanoseconds", pattern: "%Y-%m-%d %H:%M:%S.%9f", want: "2006-01-02 15:04:05.000000000"},
		{name: "syslog-style", pattern: "%b %e %H:%M:%S", want: "Jan _2 15:04:05"},
		{name: "12-hour with meridiem", pattern: "%Y-%m-%d %I:%M:%S %p", want: "2006-01-02 03:04:05 PM"},
		{name: "weekday names", pattern: "%A, %B %d %Y", want: "Monday, January 02 2006"},
		{name: "abbreviated weekday/month", pattern: "%a %b %d", want: "Mon Jan 02"},
		{name: "2-digit year", pattern: "%y-%m-%d", want: "06-01-02"},
		{name: "literal percent", pattern: "100%% done %Y", want: "100% done 2006", skipRoundTrip: true},
		{name: "timezone name", pattern: "%Y-%m-%d %H:%M:%S %Z", want: "2006-01-02 15:04:05 MST"},
		{name: "trailing percent is not a directive", pattern: "%Y-%m-%d%", wantErr: true},
		{name: "unknown directive", pattern: "%Y-%m-%Q", wantErr: true},
		{name: "day of year is unsupported — Go has no layout token for it", pattern: "%Y-%j", wantErr: true},
		{name: "%3 not followed by f", pattern: "%S.%3d", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := strftimeToGoLayout(tc.pattern)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("strftimeToGoLayout(%q) = %q, want error", tc.pattern, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("strftimeToGoLayout(%q) unexpected error: %v", tc.pattern, err)
			}
			if got != tc.want {
				t.Errorf("strftimeToGoLayout(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
			if tc.skipRoundTrip {
				return
			}
			// The whole point: the derived layout must actually round-trip
			// through time.Parse, not just look plausible.
			if _, err := time.Parse(got, time.Now().Format(got)); err != nil {
				t.Errorf("derived layout %q does not round-trip through time.Parse: %v", got, err)
			}
		})
	}
}

// fakeClassifier lets ClassifyAsync/validateClassification be tested
// without a live Bedrock call.
type fakeClassifier struct {
	result *ClassifierResult
	err    error
}

func (f *fakeClassifier) Classify(ctx context.Context, det *Detection) (*ClassifierResult, error) {
	return f.result, f.err
}

func TestValidateClassification_AcceptsVerifiedCandidate(t *testing.T) {
	lines := []string{
		`level=SEVERE trace=abc123 msg=one`,
		`level=SEVERE trace=def456 msg=two`,
		`level=SEVERE trace=ghi789 msg=three`,
	}
	tool := &classifyToolResponse{
		Level: &tagField{Present: true, FieldIndex: 1, ExtractPattern: `level=(\S+)`, SampleValue: "SEVERE"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.Level == nil {
		t.Fatal("expected a verified level candidate, got nil")
	}
	if got.Level.FieldIndex != 1 || got.Level.Pattern != `level=(\S+)` {
		t.Errorf("got %+v", got.Level)
	}
}

func TestValidateClassification_DropsUnverifiedCandidate(t *testing.T) {
	// The model claims field 1 holds the level, but it never actually
	// matches the pattern on any sample line — a hallucinated candidate
	// must not survive validation.
	lines := []string{
		`totally unrelated text one`,
		`totally unrelated text two`,
		`totally unrelated text three`,
	}
	tool := &classifyToolResponse{
		Level: &tagField{Present: true, FieldIndex: 1, ExtractPattern: `level=(\S+)`, SampleValue: "SEVERE"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.Level != nil {
		t.Errorf("expected unverified level candidate to be dropped, got %+v", got.Level)
	}
}

func TestValidateClassification_JSONPathMode(t *testing.T) {
	lines := []string{
		`{"context":{"trace":{"id":"t-1"}}}`,
		`{"context":{"trace":{"id":"t-2"}}}`,
		`{"context":{"trace":{"id":"t-3"}}}`,
	}
	tool := &classifyToolResponse{
		TraceID: &tagField{Present: true, JSONPath: ".context.trace.id", SampleValue: "t-1"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.TraceID == nil || got.TraceID.JSONPath != ".context.trace.id" {
		t.Fatalf("got %+v", got.TraceID)
	}
}

func TestValidateClassification_WholeLineSearchMode(t *testing.T) {
	lines := []string{
		`SELECT * FROM x WHERE a=1 trace_id=w1`,
		`SELECT * FROM y JOIN z WHERE b=2 AND c=3 trace_id=w2`,
		`DELETE FROM x trace_id=w3`,
	}
	tool := &classifyToolResponse{
		TraceID: &tagField{Present: true, FieldIndex: 0, ExtractPattern: `trace_id=(\S+)`, SampleValue: "w1"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.TraceID == nil {
		t.Fatal("expected a verified whole-line-search trace ID candidate")
	}
	if got.TraceID.FieldIndex != 0 || got.TraceID.JSONPath != "" || got.TraceID.Pattern != `trace_id=(\S+)` {
		t.Errorf("got %+v", got.TraceID)
	}
}

func TestValidateClassification_TimestampRoundTrips(t *testing.T) {
	lines := []string{
		`2026-09-24T10:15:03Z INFO one`,
		`2026-09-24T10:16:04Z INFO two`,
		`2026-09-24T10:17:05Z INFO three`,
	}
	tool := &classifyToolResponse{
		Timestamp: &timestampField{Present: true, FieldIndex: 1, Strftime: "%Y-%m-%dT%H:%M:%SZ", SampleValue: "2026-09-24T10:15:03Z"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.Timestamp == nil {
		t.Fatal("expected a verified timestamp candidate")
	}
	if _, err := time.Parse(got.Timestamp.Format, "2026-09-24T10:18:06Z"); err != nil {
		t.Errorf("derived format %q doesn't parse: %v", got.Timestamp.Format, err)
	}
}

func TestValidateClassification_BadStrftimeDropsTimestamp(t *testing.T) {
	lines := []string{`2026-09-24T10:15:03Z INFO one`}
	tool := &classifyToolResponse{
		Timestamp: &timestampField{Present: true, FieldIndex: 1, Strftime: "%Q-bad", SampleValue: "x"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.Timestamp != nil {
		t.Errorf("expected bad strftime to drop the candidate, got %+v", got.Timestamp)
	}
}

func TestClassifyAsync_SuccessAppliesClassification(t *testing.T) {
	lines := []string{
		`level=SEVERE msg=one`,
		`level=SEVERE msg=two`,
		`level=SEVERE msg=three`,
	}
	classifier := &fakeClassifier{
		result: &ClassifierResult{
			Level: &FieldCandidate{FieldIndex: 1, Pattern: `level=(\w+)`, Sample: "SEVERE"},
		},
	}

	done := make(chan *Detection, 1)
	det := ClassifyAsync(context.Background(), classifier, lines, true, func(d *Detection) { done <- d })
	if det == nil {
		t.Fatal("ClassifyAsync returned nil immediate Detection")
	}

	select {
	case final := <-done:
		if len(final.LevelCandidates) != 1 || final.LevelCandidates[0].FieldIndex != 1 {
			t.Errorf("onComplete Detection.LevelCandidates = %+v, want the classifier's candidate", final.LevelCandidates)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onComplete was never called")
	}
}

func TestClassifyAsync_ErrorFallsBackToHeuristic(t *testing.T) {
	// A recurring set the structural heuristic can find on its own: proves the
	// fallback path runs detectCategorical, not that it silently returns nothing.
	lines := []string{
		`2026-09-24 10:15:03 DEBUG one`,
		`2026-09-24 10:16:04 INFO two`,
		`2026-09-24 10:17:05 DEBUG three`,
		`2026-09-24 10:18:06 INFO four`,
	}
	classifier := &fakeClassifier{err: errors.New("bedrock unavailable")}

	done := make(chan *Detection, 1)
	ClassifyAsync(context.Background(), classifier, lines, true, func(d *Detection) { done <- d })

	select {
	case final := <-done:
		if len(final.LevelCandidates) == 0 {
			t.Error("expected heuristic fallback to still find the recurring-set candidate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onComplete was never called")
	}
}

func TestClassifyAsync_NotFinalSkipsClassification(t *testing.T) {
	classifier := &fakeClassifier{result: &ClassifierResult{Level: &FieldCandidate{FieldIndex: 1}}}
	called := false
	det := ClassifyAsync(context.Background(), classifier, []string{"a b c"}, false, func(d *Detection) { called = true })
	if det == nil {
		t.Fatal("expected a non-nil Detection for a partial sample")
	}
	// Give a would-be goroutine a moment; there shouldn't be one.
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("onComplete should not be called for a non-final (partial) sample")
	}
}

// A trace ID that appears only on request lines (health checks and startup
// chatter carry none) is still a real trace ID. Tag fields are verified by
// hits, not by coverage; only the level, which every line has, is held to a
// coverage bar.
func TestValidateClassification_MinorityTagFieldSurvives(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "GET /healthz 200")
	}
	lines[3] = "GET /api/items 200 trace_id=aa11"
	lines[9] = "GET /api/orders 500 trace_id=bb22"
	lines[15] = "GET /api/users 200 trace_id=cc33"

	tool := &classifyToolResponse{
		TraceID: &tagField{Present: true, ExtractPattern: `trace_id=(\S+)`, SampleValue: "aa11"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.TraceID == nil {
		t.Fatal("a trace id on 3 of 20 lines must survive validation")
	}
}

func TestValidateClassification_SingleHitNeverVerifies(t *testing.T) {
	lines := []string{"a", "b trace_id=aa11", "c", "d"}
	tool := &classifyToolResponse{
		TraceID: &tagField{Present: true, ExtractPattern: `trace_id=(\S+)`, SampleValue: "aa11"},
	}
	if got := validateClassification(context.Background(), tool, lines); got.TraceID != nil {
		t.Errorf("one hit is a coincidence, not a pattern; got %+v", got.TraceID)
	}
}

func TestValidateClassification_LevelStillNeedsCoverage(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "plain line")
	}
	lines[2] = "level=INFO x"
	lines[8] = "level=WARN y"
	lines[14] = "level=INFO z"
	tool := &classifyToolResponse{
		Level: &tagField{Present: true, ExtractPattern: `level=(\S+)`, SampleValue: "INFO"},
	}
	if got := validateClassification(context.Background(), tool, lines); got.Level != nil {
		t.Errorf("a level on 3 of 20 lines is not the line's level field; got %+v", got.Level)
	}
}

// A duration the model proposes must be something the real parser can turn
// into an integer; a pattern that captures non-numeric text extracts nothing
// at ingest, so verification must reject it.
func TestValidateClassification_DurationMustParseAsInteger(t *testing.T) {
	lines := []string{
		"span done name=a duration_ns=abc",
		"span done name=b duration_ns=def",
		"span done name=c duration_ns=ghi",
	}
	tool := &classifyToolResponse{
		DurationNs: &tagField{Present: true, ExtractPattern: `duration_ns=(\S+)`, SampleValue: "abc"},
	}
	if got := validateClassification(context.Background(), tool, lines); got.DurationNs != nil {
		t.Errorf("non-integer duration verified: %+v", got.DurationNs)
	}

	good := []string{"x duration_ns=120", "y duration_ns=3400", "z duration_ns=7"}
	tool.DurationNs.SampleValue = "120"
	if got := validateClassification(context.Background(), tool, good); got.DurationNs == nil {
		t.Error("an integer duration must verify")
	}
}

// Everything a classifier proposal is verified with is the real parser, so
// an accepted candidate is by construction one the parser extracts: spot
// check that against the candidate's own pattern on a positional level.
func TestValidateClassification_AcceptedCandidatesExtractWithRealParser(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] a",
		"[2026-10-15 12:00:02,456: WARNING/ForkPoolWorker-1] b",
		"[2026-10-15 12:00:03,789: ERROR/MainProcess] c",
	}
	tool := &classifyToolResponse{
		Level:     &tagField{Present: true, FieldIndex: 3, ExtractPattern: `^(\w+)/`, SampleValue: "INFO"},
		Timestamp: &timestampField{Present: true, FieldIndex: 1, SpanFields: 2, Strftime: "%Y-%m-%d %H:%M:%S,%3f", SampleValue: "2026-10-15 12:00:01,123"},
	}
	got := validateClassification(context.Background(), tool, lines)
	if got.Level == nil || got.Timestamp == nil {
		t.Fatalf("level=%+v timestamp=%+v, want both verified", got.Level, got.Timestamp)
	}
	p, err := NewParser(&Schema{Format: "text", LevelField: "3", LevelPattern: got.Level.Pattern,
		TimestampField: "1", TimestampFields: got.Timestamp.SpanFields, TimestampFormat: got.Timestamp.Format})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := p.ParseLine(lines[1], probeSentinel)
	if row.Level != "WARNING" || row.TsUnixNs == probeSentinel.UnixNano() {
		t.Errorf("real parser: level=%q ts=%d", row.Level, row.TsUnixNs)
	}
}

func TestProposalsToExtractors(t *testing.T) {
	resp := &proposeToolResponse{Candidates: []proposedExtractor{
		{ExtractPattern: `id[=:]\s*(\w+)`},                 // whole-line
		{FieldIndex: 3, ExtractPattern: `^(\w+)/`},         // positional + pattern
		{JSONPath: ".context.trace.id"},                    // JSON path
		{},                                                 // nothing: dropped
		{ExtractPattern: `a(b)`}, {ExtractPattern: `c(d)`}, // over the cap
	}}
	lines := []string{"[a b INFO/MainProcess] x", "[a b WARNING/Worker-1] y", "[a b ERROR/MainProcess] z"}
	got := proposalsToExtractors(lines, resp, ProbeTraceID)
	want := []ProbeExtractor{
		{Field: "none", Pattern: `id[=:]\s*(\w+)`},
		{Field: "3", Pattern: `^(\w+)/`, Spec: &ExtractSpec{Suffix: "]"}}, // the "]" every line shares after the field
		{Field: ".context.trace.id"},
	}
	if len(got) != maxProposals {
		t.Fatalf("got %d proposals, want the cap of %d: %+v", len(got), maxProposals, got)
	}
	for i, w := range want {
		if !reflect.DeepEqual(got[i], w) {
			t.Errorf("candidate %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestProposalsToExtractorsTimestamp(t *testing.T) {
	resp := &proposeToolResponse{Candidates: []proposedExtractor{
		{FieldIndex: 1, SpanFields: 2, Strftime: "%Y-%m-%d %H:%M:%S,%3f"},
		{JSONPath: ".ts", Strftime: "%Y-%m-%dT%H:%M:%S%z"},
		{FieldIndex: 1, Strftime: "%j"}, // unsupported directive: dropped
	}}
	lines := []string{"[2026-10-15 12:00:01,123: INFO a", "[2026-10-15 12:00:02,456: INFO b"}
	got := proposalsToExtractors(lines, resp, ProbeTimestamp)
	if len(got) != 2 {
		t.Fatalf("got %+v, want the two usable timestamp proposals", got)
	}
	if got[0].Field != "1" || got[0].TimestampFields != 2 || got[0].TimestampFormat != "2006-01-02 15:04:05,000" {
		t.Errorf("positional timestamp = %+v", got[0])
	}
	if got[0].Spec == nil || got[0].Spec.Prefix != "[" || got[0].Spec.Suffix != ":" {
		t.Errorf("the frame around the timestamp is learned from the lines, got %+v", got[0].Spec)
	}
	if got[1].Field != ".ts" {
		t.Errorf("json timestamp = %+v", got[1])
	}
}

func TestBuildProposePromptShowsMarks(t *testing.T) {
	lines := []string{"handled id=ab12 ok", "handled ID: cd34 ok", "handled nothing here"}
	prompt := buildProposePrompt(ProposeInput{
		Field: ProbeTraceID, Lines: lines,
		Examples:     []ProbeMark{{Line: 0, Prefix: "handled id=", Text: "ab12"}, {Line: 1, Prefix: "handled ID: ", Text: "cd34"}},
		NoValueLines: []int{2},
		Previous:     &ProbeExtractor{Field: "none", Pattern: `id=(\w+)`},
	})
	for _, want := range []string{
		"trace ID", "1: handled id=ab12 ok", `line 1: the value is "ab12"`, `line 2: the value is "cd34"`,
		"line 3 has no value", `id=(\w+)`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// jsonDoc stands in for the Smithy document Bedrock returns as a tool's
// input: all the code under test needs from it is its JSON form.
type jsonDoc struct{ json string }

func (d jsonDoc) MarshalSmithyDocument() ([]byte, error) { return []byte(d.json), nil }

// The model's tool input arrives as a Smithy document, and decoding it into
// the response structs has to keep multi-word keys (field_index,
// extract_pattern, ...). The document type's own UnmarshalSmithyDocument
// ignores `json` tags and silently zeroed every one of them, so every
// proposal arrived with nothing to locate it by and was dropped.
func TestDecodeToolInputKeepsMultiWordKeys(t *testing.T) {
	var out classifyToolResponse
	err := decodeToolInput(jsonDoc{`{
		"level": {"present": true, "field_index": 3, "json_path": "", "extract_pattern": "", "sample_value": "INFO"},
		"trace_id": {"present": true, "field_index": 0, "json_path": "", "extract_pattern": "trace_id=(\\S+)", "sample_value": "abc"},
		"timestamp": {"present": true, "field_index": 1, "span_fields": 2, "json_path": "", "strftime": "%Y-%m-%d %H:%M:%S", "sample_value": "2026-10-15 12:00:00"}
	}`}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Level == nil || out.Level.FieldIndex != 3 || out.Level.SampleValue != "INFO" {
		t.Errorf("level = %+v, want field_index 3 and sample INFO", out.Level)
	}
	if out.TraceID == nil || out.TraceID.ExtractPattern != `trace_id=(\S+)` {
		t.Errorf("trace = %+v, want its extract_pattern", out.TraceID)
	}
	if out.Timestamp == nil || out.Timestamp.FieldIndex != 1 || out.Timestamp.SpanFields != 2 {
		t.Errorf("timestamp = %+v, want field_index 1 spanning 2", out.Timestamp)
	}

	var pr proposeToolResponse
	if err := decodeToolInput(jsonDoc{`{"candidates":[{"field_index":0,"json_path":".a.b","extract_pattern":"","span_fields":0,"strftime":""}]}`}, &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Candidates) != 1 || pr.Candidates[0].JSONPath != ".a.b" {
		t.Errorf("proposals = %+v, want the json_path kept", pr.Candidates)
	}
}

// Models miscount whitespace-delimited fields, so the prompt spells the
// numbering out for a few lines instead of leaving it to be counted.
func TestFieldTable(t *testing.T) {
	got := fieldTable("2026-10-15 12:00:00.000 INFO handled trace_id=abc")
	want := "1=2026-10-15 2=12:00:00.000 3=INFO 4=handled 5=trace_id=abc"
	if got != want {
		t.Errorf("fieldTable = %q, want %q", got, want)
	}
	if !strings.Contains(fieldTable(strings.Repeat("x", 100)), "...") {
		t.Error("a very long field must be truncated")
	}
}

func TestClassifyPromptIncludesFieldNumbering(t *testing.T) {
	lines := []string{"2026-10-15 12:00:00 INFO a", "2026-10-15 12:00:01 INFO b"}
	det := Detect(lines)
	p := buildClassifyPrompt(det, lines)
	if !strings.Contains(p, "1=2026-10-15 2=12:00:00 3=INFO 4=a") {
		t.Errorf("prompt lacks the field numbering:\n%s", p)
	}
}

// A model that cannot find a trace ID may point at the timestamp instead.
// Trace IDs verify on any hit count, so only the value itself can tell them
// apart: a date or time is not an identifier.
func TestValidateClassification_TraceIDIsNotATimestamp(t *testing.T) {
	lines := []string{
		`2026/10/16 14:52:08 ingest-stream: received 13 rows from customer=graybeard`,
		`2026/10/16 14:52:11 ingest-stream: received 871 rows from customer=loom`,
		`2026/10/16 14:52:14 sample: customer=graybeard namespace=anterior app=django lines=0 partial=true`,
	}
	for name, f := range map[string]*tagField{
		"date field":     {Present: true, FieldIndex: 1, SampleValue: "2026/10/16"},
		"time field":     {Present: true, FieldIndex: 2, SampleValue: "14:52:08"},
		"date and time":  {Present: true, ExtractPattern: `^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d)`, SampleValue: "2026/10/16 14:52:08"},
		"time in a line": {Present: true, ExtractPattern: `(\d\d:\d\d:\d\d)`, SampleValue: "14:52:08"},
	} {
		for _, role := range []string{"trace_id", "span_id", "parent_span_id"} {
			tool := &classifyToolResponse{}
			switch role {
			case "trace_id":
				tool.TraceID = f
			case "span_id":
				tool.SpanID = f
			default:
				tool.ParentSpanID = f
			}
			got := validateClassification(context.Background(), tool, lines)
			if got.TraceID != nil || got.SpanID != nil || got.ParentSpanID != nil {
				t.Errorf("%s as %s: a timestamp must not pass for an identifier: %+v %+v %+v", name, role, got.TraceID, got.SpanID, got.ParentSpanID)
			}
		}
	}
}
