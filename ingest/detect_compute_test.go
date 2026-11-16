package ingest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// A span-emitting service whose multi-line errors leave continuation lines
// with no timestamp. Heuristic detection (no model) must still find the
// timestamp, since every wizard suggestion hangs off it.
func TestDetectTimestampWithContinuationLines(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		ts := time.Date(2026, 10, 16, 9, 42, i*3, 0, time.UTC).Format("2006/01/02 15:04:05")
		lines = append(lines,
			ts+" span start name=compute.ExecuteQueryStream trace_id="+fmt.Sprintf("%032x", i+1),
			ts+" cold shard shard-1: lambda returned 500: query failed: IO Error: No files found",
			"LINE 1: ... raw, ts FROM read_parquet('s3://weft-cold/x')",
			"^",
			ts+" span done name=compute.ExecuteQueryStream trace_id="+fmt.Sprintf("%032x", i+1)+" duration=1.5s duration_ns=1500000000 status=ok",
		)
	}
	d := Detect(lines)
	t.Logf("format=%s timestamps=%+v level=%+v", d.SuggestFormat(), d.TimestampCandidates, d.LevelCandidates)
	if len(DetectedExtractors(d, ProbeTimestamp)) == 0 {
		t.Fatal("no timestamp candidate")
	}
}

// The CLI wizard receives detection as the JSON the server stored; the
// suggestions must survive that trip.
func TestDetectionJSONRoundTripKeepsCandidates(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		ts := time.Date(2026, 10, 16, 9, 42, i*3, 0, time.UTC).Format("2006/01/02 15:04:05")
		lines = append(lines, ts+" span done name=x trace_id="+fmt.Sprintf("%032x", i+1)+" duration_ns=15 status=ok")
	}
	d := Detect(lines)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var back Detection
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, want := len(DetectedExtractors(&back, ProbeTimestamp)), len(DetectedExtractors(d, ProbeTimestamp)); got == 0 || got != want {
		t.Fatalf("timestamp candidates after JSON round trip: %d, before: %d\n%s", got, want, b)
	}
}
