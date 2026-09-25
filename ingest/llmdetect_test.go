package ingest

import (
	"context"
	"errors"
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
	got := validateClassification(tool, lines)
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
	got := validateClassification(tool, lines)
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
	got := validateClassification(tool, lines)
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
	got := validateClassification(tool, lines)
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
	got := validateClassification(tool, lines)
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
	got := validateClassification(tool, lines)
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
	det := ClassifyAsync(classifier, lines, true, func(d *Detection) { done <- d })
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
	// A line the fixed-vocabulary heuristic can actually find (DEBUG is
	// in levelStrings) — proves the fallback path runs detectLevels,
	// not that it silently returns nothing.
	lines := []string{
		`2026-09-24 10:15:03 DEBUG one`,
		`2026-09-24 10:16:04 DEBUG two`,
		`2026-09-24 10:17:05 DEBUG three`,
	}
	classifier := &fakeClassifier{err: errors.New("bedrock unavailable")}

	done := make(chan *Detection, 1)
	ClassifyAsync(classifier, lines, true, func(d *Detection) { done <- d })

	select {
	case final := <-done:
		if len(final.LevelCandidates) == 0 {
			t.Error("expected heuristic fallback to still find the DEBUG level candidate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onComplete was never called")
	}
}

func TestClassifyAsync_NotFinalSkipsClassification(t *testing.T) {
	classifier := &fakeClassifier{result: &ClassifierResult{Level: &FieldCandidate{FieldIndex: 1}}}
	called := false
	det := ClassifyAsync(classifier, []string{"a b c"}, false, func(d *Detection) { called = true })
	if det == nil {
		t.Fatal("expected a non-nil Detection for a partial sample")
	}
	// Give a would-be goroutine a moment; there shouldn't be one.
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Error("onComplete should not be called for a non-final (partial) sample")
	}
}
