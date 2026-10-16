package ingest

import "testing"

func TestDurationModeParse(t *testing.T) {
	cases := []struct {
		unit, text string
		want       int64
		ok         bool
	}{
		{"", "5015996546", 5015996546, true},
		{"ns", "42", 42, true},
		{"ns", "1.5", 0, false}, // nanoseconds are a whole count
		{"us", "12", 12_000, true},
		{"ms", "12.5", 12_500_000, true},
		{"s", "5.015996546", 5_015_996_546, true},
		{"s", "-1", 0, false},
		{"ms", "NaN", 0, false},
		{"ms", "Inf", 0, false},
		{"text", "5.015996546s", 5_015_996_546, true},
		{"text", "340ms", 340_000_000, true},
		{"text", "1m2s", 62_000_000_000, true},
		{"text", "12µs", 12_000, true},
		{"text", "340", 0, false}, // no unit: ambiguous, so not a duration
		{"text", "-5s", 0, false},
	}
	for _, c := range cases {
		m, ok := durationUnitMode(c.unit)
		if !ok {
			t.Fatalf("unit %q rejected", c.unit)
		}
		if got, ok := m.parse(c.text); ok != c.ok || got != c.want {
			t.Errorf("unit %q parse(%q) = %d,%v; want %d,%v", c.unit, c.text, got, ok, c.want, c.ok)
		}
	}
	if _, ok := durationUnitMode("minutes"); ok {
		t.Error("unknown unit accepted")
	}
}

func TestDurationUnitValidatedAndGated(t *testing.T) {
	s := &Schema{Format: "text", DurationNsField: "5", DurationUnit: "minutes"}
	found := false
	for _, p := range s.Validate() {
		found = found || contains(p, "duration_unit")
	}
	if !found {
		t.Errorf("bad duration_unit not reported: %v", s.Validate())
	}
	if got := (&Schema{DurationUnit: "ms"}).MinAgentVersion(); got != MinAgentVersionForDurationUnit {
		t.Errorf("ms needs %q, got %q", MinAgentVersionForDurationUnit, got)
	}
	if got := (&Schema{DurationUnit: "ns"}).MinAgentVersion(); got != "" {
		t.Errorf("ns reads on any agent, got %q", got)
	}
	if got := (&Schema{Extract: map[string]ExtractSpec{"timestamp": {}}}).MinAgentVersion(); got != MinAgentVersionForExtract {
		t.Errorf("extract needs %q, got %q", MinAgentVersionForExtract, got)
	}
}

// A bare number whose unit the caller supplies: the same highlighted text
// means different nanoseconds in different units.
func TestProbeBareDurationUsesCallerUnit(t *testing.T) {
	lines := []string{
		"2026-10-16 09:00:00 handled GET /a took=12.5",
		"2026-10-16 09:00:01 handled GET /b",
		"2026-10-16 09:00:02 handled GET /c took=340",
	}
	res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: lines, DurationUnit: "ms", Marks: []ProbeMark{markAt(t, lines, 0, "12.5")}})
	wantValues(t, res, map[int]string{0: "12500000", 2: "340000000"})
	if res.Extractor.DurationUnit != "ms" {
		t.Errorf("unit = %q, want ms", res.Extractor.DurationUnit)
	}
}

var spanDoneLines = []string{
	"2026/10/16 09:42:36 span start name=q trace_id=0d11 span_id=f20a",
	"2026/10/16 09:42:41 cold shard shard-1: lambda returned 500",
	"LINE 1: ... FROM read_parquet('s3://x')",
	"2026/10/16 09:42:41 span done name=q trace_id=0d11 span_id=f20a duration=5.015996546s duration_ns=5015996546 status=ok",
	"2026/10/16 09:42:58 span done name=q trace_id=5133 span_id=7e40 duration=1.147619075s duration_ns=1147619075 status=ok",
}

// With nothing marked, the model's proposal (with its unit) is verified by
// the parser and returned as a first guess.
func TestProbeProposeFirstGuessUsesModelsUnit(t *testing.T) {
	p := &fakeProposer{cands: []ProbeExtractor{
		{Field: "none", Pattern: `duration=(\S+)`, DurationUnit: "text"},
	}}
	res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: spanDoneLines, Propose: true, Proposer: p})
	wantValues(t, res, map[int]string{3: "5015996546", 4: "1147619075"})
	if res.Source != "model" || res.Extractor.DurationUnit != "text" {
		t.Errorf("source=%q unit=%q, want model/text", res.Source, res.Extractor.DurationUnit)
	}
	if len(p.got.Examples) != 0 || len(p.got.NoValueLines) != 0 {
		t.Errorf("a first guess must not claim marks: %+v", p.got)
	}
}

// The wrong unit makes the same pattern read nothing, so it is not offered.
func TestProbeProposeFirstGuessRejectsWrongUnit(t *testing.T) {
	p := &fakeProposer{cands: []ProbeExtractor{
		{Field: "none", Pattern: `duration=(\S+)`, DurationUnit: "ms"}, // "5.01s" is not a bare number
	}}
	res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: spanDoneLines, Propose: true, Proposer: p})
	if res.Extractor != nil {
		t.Fatalf("offered %+v although it reads no line", res.Extractor)
	}
}

func TestProbeProposeWithoutProposerOffersNothing(t *testing.T) {
	res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: spanDoneLines, Propose: true})
	if res.Extractor != nil || res.Message == "" {
		t.Fatalf("res = %+v, want no extractor and a message", res)
	}
}

// A bare fractional number is located but flagged: only the person knows the unit.
func TestProbeBareDurationNeedsUnit(t *testing.T) {
	lines := []string{"a took=12.5", "b", "c took=340"}
	res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: lines, Marks: []ProbeMark{markAt(t, lines, 0, "12.5")}})
	if !res.NeedsUnit {
		t.Error("NeedsUnit not set for a bare number with no unit given")
	}
	res = probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: lines, DurationUnit: "ms", Marks: []ProbeMark{markAt(t, lines, 0, "12.5")}})
	if res.NeedsUnit {
		t.Error("NeedsUnit set although the unit was given")
	}
}
