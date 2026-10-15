package ingest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// markAt builds a positive mark for the first occurrence of text in lines[line].
func markAt(t *testing.T, lines []string, line int, text string) ProbeMark {
	t.Helper()
	i := strings.Index(lines[line], text)
	if i < 0 {
		t.Fatalf("line %d %q has no %q", line, lines[line], text)
	}
	return ProbeMark{Line: line, Prefix: lines[line][:i], Text: text}
}

func probe(t *testing.T, req ProbeRequest) *ProbeResult {
	t.Helper()
	res, err := Probe(req)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return res
}

func wantValues(t *testing.T, res *ProbeResult, want map[int]string) {
	t.Helper()
	if res.Extractor == nil {
		t.Fatalf("no extractor: %s", res.Message)
	}
	t.Logf("extractor: %+v", *res.Extractor)
	for _, r := range res.Results {
		if w := want[r.Line]; r.Value != w {
			t.Errorf("line %d: value %q, want %q (extractor %+v)", r.Line, r.Value, w, res.Extractor)
		}
	}
}

// A trace tag on only some lines (health checks carry none) is extracted with
// a whole-line pattern, and the lines without it are reported unmatched.
func TestProbeTraceTagOnMinorityOfLines(t *testing.T) {
	lines := []string{
		"2026-10-15 12:00:00.100 DEBUG handled GET /healthz 200 1ms",
		"2026-10-15 12:00:00.200 INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a871",
		"2026-10-15 12:00:00.300 DEBUG handled GET /healthz 200 1ms",
		"2026-10-15 12:00:00.400 INFO handled GET /api/orders 500 90ms trace_id=aa01bb02cc03dd04",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 1, "3f9a1c0de4b2a871")},
	})
	wantValues(t, res, map[int]string{1: "3f9a1c0de4b2a871", 3: "aa01bb02cc03dd04"})
	if res.Extractor.Pattern != "" || res.Extractor.Spec == nil || res.Extractor.Spec.Prefix != "trace_id=" {
		t.Errorf("extractor = %+v, want a field read by position, framed by the required prefix trace_id=", res.Extractor)
	}
	if res.Matched != 2 || res.Total != 4 {
		t.Errorf("matched %d/%d, want 2/4", res.Matched, res.Total)
	}
}

// The loosest anchor (a bare "[") would pull the level out of "[INFO]"; the
// probe must widen until the highlighted request id is what comes out.
func TestProbeWidensPastAmbiguousDelimiter(t *testing.T) {
	lines := []string{
		"2026-10-15 12:00:00,100 [INFO] [req-8f3a21c4] user.login ok in 3ms",
		"2026-10-15 12:00:00,200 [WARNING] [req-0b9d77e1] user.update ok in 40ms",
		"2026-10-15 12:00:00,300 [INFO] booting worker",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "req-8f3a21c4")},
	})
	wantValues(t, res, map[int]string{0: "req-8f3a21c4", 1: "req-0b9d77e1", 2: ""})
}

// Flagging a line that read a value it should not narrows the rule. Here
// structure cannot tell the two lines apart (the same field, framed the same
// way), so the probe falls back to a regex that can.
func TestProbeNegativeMarkTightensPattern(t *testing.T) {
	lines := []string{
		"worker req [deadbeef] ok",
		"build job [cafebabe] ok",
		"worker req [0123abcd] ok",
	}
	marks := []ProbeMark{markAt(t, lines, 0, "deadbeef")}
	first := probe(t, ProbeRequest{Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines, Marks: marks})
	if first.Results[1].Value != "cafebabe" {
		t.Fatalf("setup: the structural rule should also read line 1, got %q (%+v)", first.Results[1].Value, first.Extractor)
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks:    append(marks, ProbeMark{Line: 1, None: true}),
		Previous: first.Extractor,
	})
	wantValues(t, res, map[int]string{0: "deadbeef", 1: "", 2: "0123abcd"})
	if res.Extractor.Field != "none" {
		t.Errorf("only a regex separates these lines, got %+v", res.Extractor)
	}
}

// Flagging a line the pattern missed (with its value) makes the pattern cover it.
func TestProbePositiveMarkWidensPattern(t *testing.T) {
	lines := []string{
		"INFO handled request_id=a1b2c3d4",
		"INFO handled req=ffeeddcc",
	}
	marks := []ProbeMark{markAt(t, lines, 0, "a1b2c3d4")}
	first := probe(t, ProbeRequest{Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines, Marks: marks})
	if first.Results[1].Matched {
		t.Fatalf("setup: line 1 should be missed by the first pattern, got %+v", first.Extractor)
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks:    append(marks, markAt(t, lines, 1, "ffeeddcc")),
		Previous: first.Extractor,
	})
	wantValues(t, res, map[int]string{0: "a1b2c3d4", 1: "ffeeddcc"})
}

// In JSON lines the value is located by path, not regex.
func TestProbeJSONPath(t *testing.T) {
	lines := []string{
		`{"ts":"2026-10-15T12:00:00Z","msg":"booting"}`,
		`{"ts":"2026-10-15T12:00:01Z","level":"info","msg":"request","context":{"trace":{"id":"4bf92f3577b34da6a3ce929d0e0e4736"}}}`,
		`{"ts":"2026-10-15T12:00:02Z","level":"warn","msg":"request","context":{"trace":{"id":"00112233445566778899aabbccddeeff"}}}`,
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "json"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 1, "4bf92f3577b34da6a3ce929d0e0e4736")},
	})
	wantValues(t, res, map[int]string{0: "", 1: "4bf92f3577b34da6a3ce929d0e0e4736", 2: "00112233445566778899aabbccddeeff"})
	if res.Extractor.Field != ".context.trace.id" || res.Extractor.Pattern != "" {
		t.Errorf("extractor = %+v, want JSON path .context.trace.id", res.Extractor)
	}
}

// Level has no whole-line mode, so it is a positional field. When the field
// carries more than the level (Celery's INFO/MainProcess), the text after the
// highlight is not constant across lines, so the rule cuts at its delimiter
// instead of stripping a literal suffix.
func TestProbeLevelPositionalCutsAtDelimiter(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] Task a.b[1] received",
		"[2026-10-15 12:00:02,456: WARNING/ForkPoolWorker-1] Task a.b[1] slow",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeLevel, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "INFO")},
	})
	wantValues(t, res, map[int]string{0: "INFO", 1: "WARNING"})
	if res.Extractor.Field != "3" || res.Extractor.Spec == nil || res.Extractor.Spec.CutAt != "/" || res.Extractor.Spec.Suffix != "" {
		t.Errorf("extractor = %+v spec=%+v, want field 3 cutting at /", res.Extractor, res.Extractor.Spec)
	}
	// The highlight is what was read: the level, nothing around it.
	if r := res.Results[1]; r.Text != "WARNING" || r.Prefix != "[2026-10-15 12:00:02,456: " {
		t.Errorf("highlight = %q after %q", r.Text, r.Prefix)
	}
}

func TestProbeLevelWholeField(t *testing.T) {
	lines := []string{
		"2026-10-15 12:00:00.100 INFO started",
		"2026-10-15 12:00:00.200 ERROR failed",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeLevel, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "INFO")},
	})
	wantValues(t, res, map[int]string{0: "INFO", 1: "ERROR"})
	if res.Extractor.Field != "3" || res.Extractor.Pattern != "" || res.Extractor.Spec == nil {
		t.Errorf("extractor = %+v, want field 3 read by an (empty) spec", res.Extractor)
	}
}

func TestProbeTimestamp(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] first",
		"[2026-10-15 12:00:02,456: INFO/MainProcess] second",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTimestamp, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "2026-10-15 12:00:01,123")},
	})
	if res.Extractor == nil {
		t.Fatalf("no extractor: %s", res.Message)
	}
	if res.Extractor.Field != "1" || res.Extractor.TimestampFields != 2 || res.Extractor.TimestampFormat != "2006-01-02 15:04:05,000" {
		t.Errorf("extractor = %+v, want field 1 spanning 2 with the comma-millis layout", res.Extractor)
	}
	if res.Matched != 2 {
		t.Errorf("matched %d, want 2", res.Matched)
	}
}

// Contradictory marks have no extractor; the probe says so instead of guessing.
func TestProbeUnsatisfiable(t *testing.T) {
	lines := []string{"id=abc123 ok", "id=abc123 ok"}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "abc123"), {Line: 1, None: true}},
	})
	if res.Extractor != nil || res.Message == "" {
		t.Fatalf("want no extractor and a message, got %+v %q", res.Extractor, res.Message)
	}
}

func TestProbeRejectsMarkThatDoesNotMatchTheLine(t *testing.T) {
	lines := []string{"id=abc123 ok"}
	_, err := Probe(ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{{Line: 0, Prefix: "nope ", Text: "abc123"}},
	})
	if err == nil {
		t.Fatal("want an error for a prefix/text that is not in the line")
	}
	_, err = Probe(ProbeRequest{Field: ProbeTraceID, Lines: lines, Marks: []ProbeMark{{Line: 5, Text: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "line 5") {
		t.Fatalf("want an out-of-range error naming the line, got %v", err)
	}
	_, err = Probe(ProbeRequest{Field: "bogus", Lines: lines})
	if err == nil {
		t.Fatal("want an error for an unknown field")
	}
}

// Result prefixes let a UI highlight each extracted value in its line.
func TestProbeResultsCarryHighlightPrefix(t *testing.T) {
	lines := []string{"a b trace_id=abc123 z", "a b trace_id=def456 z"}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "abc123")},
	})
	for i, r := range res.Results {
		if got := lines[i][len(r.Prefix) : len(r.Prefix)+len(r.Value)]; got != r.Value {
			t.Errorf("line %d: prefix %q does not point at value %q (got %q)", i, r.Prefix, r.Value, got)
		}
	}
	_ = fmt.Sprint
}

// With no marks, a Previous extractor is just evaluated: the wizard uses
// this to show a detected candidate on every line before the user has
// marked anything.
func TestProbeEvaluatesPreviousWithoutMarks(t *testing.T) {
	lines := []string{"a trace_id=abc123 z", "b nothing here", "c trace_id=def456 z"}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Previous: &ProbeExtractor{Field: "none", Pattern: `trace_id=(\w+)`},
	})
	wantValues(t, res, map[int]string{0: "abc123", 1: "", 2: "def456"})
	if res.Matched != 2 || res.Results[0].Prefix != "a trace_id=" {
		t.Errorf("matched=%d prefix=%q", res.Matched, res.Results[0].Prefix)
	}
	if _, err := Probe(ProbeRequest{Field: ProbeTraceID, Lines: lines}); err == nil {
		t.Error("no marks and no previous extractor must still be an error")
	}
}

func TestDetectedExtractor(t *testing.T) {
	d := &Detection{
		Lines:               []string{"2026-10-15 12:00:01 INFO a", "2026-10-15 12:00:02 WARN b"},
		TimestampCandidates: []TimestampCandidate{{FieldIndex: 1, SpanFields: 2, Format: "2006-01-02 15:04:05"}},
		LevelCandidates:     []FieldCandidate{{FieldIndex: 3}},
		TraceIDCandidates:   []FieldCandidate{{Pattern: `trace_id=(\S+)`}},
		SpanIDCandidates:    []FieldCandidate{{JSONPath: ".span.id"}},
	}
	for _, c := range []struct {
		f    ProbeField
		want ProbeExtractor
	}{
		{ProbeTimestamp, ProbeExtractor{Field: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05", Spec: &ExtractSpec{}}},
		{ProbeLevel, ProbeExtractor{Field: "3", Spec: &ExtractSpec{}}},
		{ProbeTraceID, ProbeExtractor{Field: "none", Pattern: `trace_id=(\S+)`}},
		{ProbeSpanID, ProbeExtractor{Field: ".span.id"}},
	} {
		got := DetectedExtractor(d, c.f)
		if got == nil || !reflect.DeepEqual(*got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.f, got, c.want)
		}
	}
	if DetectedExtractor(d, ProbeSpanName) != nil || DetectedExtractor(nil, ProbeLevel) != nil {
		t.Error("no candidate must give nil")
	}
}

// fakeProposer stands in for the model: it returns canned candidates and
// records how it was asked.
type fakeProposer struct {
	cands []ProbeExtractor
	err   error
	calls int
	got   ProposeInput
}

func (f *fakeProposer) ProposeExtractors(ctx context.Context, in ProposeInput) ([]ProbeExtractor, error) {
	f.calls++
	f.got = in
	return f.cands, f.err
}

// Two spellings the deterministic builder cannot unify with one anchor.
var twoSpellings = []string{
	"handled id=ab12 ok",
	"handled ID: cd34 ok",
	"handled nothing here",
}

func twoSpellingMarks(t *testing.T) []ProbeMark {
	return []ProbeMark{markAt(t, twoSpellings, 0, "ab12"), markAt(t, twoSpellings, 1, "cd34")}
}

func TestProbeDeterministicFailsOnTwoSpellings(t *testing.T) {
	res := probe(t, ProbeRequest{Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings, Marks: twoSpellingMarks(t)})
	if res.Extractor != nil {
		t.Fatalf("setup: deterministic synthesis unexpectedly succeeded: %+v", res.Extractor)
	}
}

// When the deterministic builder has nothing, the model is asked for
// candidates; a candidate is accepted only if the real parser, applied to the
// sample, satisfies every mark.
func TestProbeFallsBackToModelProposal(t *testing.T) {
	prop := &fakeProposer{cands: []ProbeExtractor{
		{Field: "none", Pattern: `nonsense=(\w+)`}, // does not satisfy the marks: ignored
		{Field: "none", Pattern: `(?i)id[=:]\s*(\w+)`},
	}}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings,
		Marks: twoSpellingMarks(t), Proposer: prop,
	})
	wantValues(t, res, map[int]string{0: "ab12", 1: "cd34", 2: ""})
	if res.Source != "model" {
		t.Errorf("source = %q, want model", res.Source)
	}
	if prop.calls != 1 {
		t.Errorf("proposer called %d times, want 1", prop.calls)
	}
	// The model is told what the user marked, so it can fit them.
	if len(prop.got.Examples) != 2 || prop.got.Examples[1].Text != "cd34" || prop.got.Field != ProbeTraceID {
		t.Errorf("proposer input = %+v", prop.got)
	}
}

func TestProbeModelProposalMustSatisfyMarks(t *testing.T) {
	prop := &fakeProposer{cands: []ProbeExtractor{{Field: "none", Pattern: `nonsense=(\w+)`}}}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings,
		Marks: twoSpellingMarks(t), Proposer: prop,
	})
	if res.Extractor != nil || res.Message == "" || res.Source != "" {
		t.Fatalf("a proposal that fails the marks must not be used: %+v", res)
	}
}

func TestProbeDoesNotAskModelWhenRulesSucceed(t *testing.T) {
	lines := []string{"a trace_id=abc123 z", "b nothing"}
	prop := &fakeProposer{}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "abc123")}, Proposer: prop,
	})
	if prop.calls != 0 || res.Source != "rule" {
		t.Errorf("calls=%d source=%q, want 0 and rule", prop.calls, res.Source)
	}
}

// A model that errors degrades to "no rule found", never to a failed request.
func TestProbeModelErrorIsNotFatal(t *testing.T) {
	prop := &fakeProposer{err: errors.New("bedrock down")}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings,
		Marks: twoSpellingMarks(t), Proposer: prop,
	})
	if res.Extractor != nil || res.Message == "" {
		t.Fatalf("got %+v", res)
	}
}

// What is highlighted as the timestamp is what the parser actually reads: the
// parser strips a field's leading "[" and trailing "]" / ":" before parsing,
// so the highlight must not include them.
func TestProbeTimestampHighlightExcludesBracketAndColon(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] first",
		"[2026-10-15 12:00:02,456: INFO/MainProcess] second",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTimestamp, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "2026-10-15 12:00:01,123")},
	})
	if res.Extractor == nil {
		t.Fatalf("no extractor: %s", res.Message)
	}
	for i, r := range res.Results {
		wantText := []string{"2026-10-15 12:00:01,123", "2026-10-15 12:00:02,456"}[i]
		if r.Text != wantText || r.Prefix != "[" {
			t.Errorf("line %d: highlight %q after prefix %q, want %q after \"[\"", i, r.Text, r.Prefix, wantText)
		}
	}

	// A bracket-and-colon-free timestamp is unaffected.
	plain := []string{"2026-10-15 12:00:01 INFO a", "2026-10-15 12:00:02 INFO b"}
	res = probe(t, ProbeRequest{
		Field: ProbeTimestamp, Base: ProbeBase{Format: "text"}, Lines: plain,
		Marks: []ProbeMark{markAt(t, plain, 0, "2026-10-15 12:00:01")},
	})
	if res.Results[1].Text != "2026-10-15 12:00:02" || res.Results[1].Prefix != "" {
		t.Errorf("plain timestamp highlight = %q after %q", res.Results[1].Text, res.Results[1].Prefix)
	}
}

// Structural punctuation around a value ("[", ":") is stripped literally;
// the key of a key=value tag is a literal prefix, and a count-from-the-end
// index finds a trailing tag however long the message is.
func TestProbeDerivesTrimsFromTheHighlight(t *testing.T) {
	lines := []string{
		"GET /a 200 trace_id=abc123",
		"GET /a-much-longer-path/with/more/words 200 trace_id=def456",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "abc123"), markAt(t, lines, 1, "def456")},
	})
	if res.Extractor == nil {
		t.Fatalf("no extractor: %s", res.Message)
	}
	wantValues(t, res, map[int]string{0: "abc123", 1: "def456"})
}

// Both ends of the line are tried: here the tag is the last field on every
// line but its index from the front varies.
func TestProbeFindsFieldCountedFromTheEnd(t *testing.T) {
	lines := []string{
		"a b trace_id=abc123",
		"a b c d e f g trace_id=def456",
		"a b c trace_id=0a1b2c",
	}
	res := probe(t, ProbeRequest{
		Field: ProbeLevel, Base: ProbeBase{Format: "text"}, Lines: lines,
		Marks: []ProbeMark{markAt(t, lines, 0, "abc123"), markAt(t, lines, 1, "def456")},
	})
	if res.Extractor == nil || res.Extractor.Field != "-1" {
		t.Fatalf("extractor = %+v (%s), want the last field (-1)", res.Extractor, res.Message)
	}
}

// A proposed regex with several capturing groups is not rejected for it: the
// group that yields what the user marked, on every marked line, is the value,
// and the others are made non-capturing so the stored rule has the one group
// ingest reads.
func TestProbeNormalizesProposalToTheMarkedGroup(t *testing.T) {
	prop := &fakeProposer{cands: []ProbeExtractor{{Field: "none", Pattern: `(id=|ID:)\s*(\w+)`}}}
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings,
		Marks: twoSpellingMarks(t), Proposer: prop,
	})
	wantValues(t, res, map[int]string{0: "ab12", 1: "cd34", 2: ""})
	if res.Source != "model" {
		t.Fatalf("source = %q, want model", res.Source)
	}
	re, err := regexp.Compile(res.Extractor.Pattern)
	if err != nil || re.NumSubexp() != 1 {
		t.Errorf("stored pattern %q has %d capture groups (%v), want exactly 1", res.Extractor.Pattern, re.NumSubexp(), err)
	}
}

// If no group of the proposal yields the marked text, it is still rejected.
func TestProbeRejectsProposalWhoseGroupsMissTheMarks(t *testing.T) {
	prop := &fakeProposer{cands: []ProbeExtractor{{Field: "none", Pattern: `(id)=(\d+)`}}} // captures ab / nothing
	res := probe(t, ProbeRequest{
		Field: ProbeTraceID, Base: ProbeBase{Format: "text"}, Lines: twoSpellings,
		Marks: twoSpellingMarks(t), Proposer: prop,
	})
	if res.Extractor != nil {
		t.Fatalf("accepted %+v", res.Extractor)
	}
}

func TestOnlyCaptureGroupKeepsTheOriginalSpelling(t *testing.T) {
	for _, c := range []struct {
		pattern string
		keep    int
		want    string
	}{
		{`(id=|ID:)\s*(\w+)`, 2, `(?:id=|ID:)\s*(\w+)`},
		{`(id=|ID:)\s*(\w+)`, 1, `(id=|ID:)\s*(?:\w+)`},
		{`\((a)\)(b)`, 2, `\((?:a)\)(b)`},           // an escaped paren is not a group
		{`[(](a)(b)`, 1, `[(](a)(?:b)`},             // nor is one inside a class
		{`(?P<k>a)(?P<v>b)`, 2, `(?:a)(?P<v>b)`},    // named groups count
		{`(?i)(a)(?:b)(c)`, 2, `(?i)(?:a)(?:b)(c)`}, // already non-capturing: left alone
	} {
		got, err := onlyCaptureGroup(c.pattern, c.keep)
		if err != nil || got != c.want {
			t.Errorf("onlyCaptureGroup(%q, %d) = %q, %v; want %q", c.pattern, c.keep, got, err, c.want)
		}
		if re, err := regexp.Compile(got); err != nil || re.NumSubexp() != 1 {
			t.Errorf("%q does not compile to exactly one group: %v", got, err)
		}
	}
}

// A regex is a last resort. Where a value is found by a field's position and
// the text that frames it, that is what the probe must produce, including when
// the value is on only some lines (the framing text is required, so a line
// without it has no value rather than a wrong one).
func TestProbeUsesNoRegexWhereStructureSuffices(t *testing.T) {
	jsonLines := []string{
		`{"ts":"2026-10-15T12:00:00Z","msg":"booting"}`,
		`{"ts":"2026-10-15T12:00:01Z","msg":"request","context":{"trace":{"id":"4bf92f3577b34da6a3ce929d0e0e4736"}}}`,
		`{"ts":"2026-10-15T12:00:02Z","msg":"tick"}`,
		`{"ts":"2026-10-15T12:00:03Z","msg":"request","context":{"trace":{"id":"00112233445566778899aabbccddeeff"}}}`,
	}
	for name, c := range map[string]struct {
		format string
		lines  []string
		mark   int
		text   string
		want   map[int]string
	}{
		"a sparse key=value tag": {"text", []string{
			"2026-10-15 12:00:00.100 DEBUG handled GET /healthz 200 1ms",
			"2026-10-15 12:00:00.200 INFO handled GET /api/items 200 12ms trace_id=3f9a1c0de4b2a871",
			"2026-10-15 12:00:00.300 DEBUG handled GET /healthz 200 1ms",
			"2026-10-15 12:00:00.400 INFO handled GET /api/orders 500 90ms trace_id=aa01bb02cc03dd04",
		}, 1, "3f9a1c0de4b2a871", map[int]string{0: "", 1: "3f9a1c0de4b2a871", 2: "", 3: "aa01bb02cc03dd04"}},
		"a bracketed id after another bracketed field": {"text", []string{
			"2026-10-15 12:00:00,100 [INFO] [req-8f3a21c4] user.login ok in 3ms",
			"2026-10-15 12:00:00,200 [WARNING] [req-0b9d77e1] user.update ok in 40ms",
			"2026-10-15 12:00:00,300 [INFO] booting worker now",
		}, 0, "req-8f3a21c4", map[int]string{0: "req-8f3a21c4", 1: "req-0b9d77e1", 2: ""}},
		"an id inside a field": {"text", []string{
			"[2026-10-15 12:00:01,123: INFO/MainProcess] Task app.send[290996dd-9de5-7981-2aaa-1b2c3d4e5f60] succeeded in 0.1s",
			"[2026-10-15 12:00:02,456: INFO/Worker-1] Task app.send[11111111-2222-3333-4444-555555555555] succeeded in 0.2s",
			"[2026-10-15 12:00:03,789: INFO/MainProcess] Starting up now",
		}, 0, "290996dd-9de5-7981-2aaa-1b2c3d4e5f60", map[int]string{0: "290996dd-9de5-7981-2aaa-1b2c3d4e5f60", 1: "11111111-2222-3333-4444-555555555555", 2: ""}},
		"a nested JSON value": {"json", jsonLines, 1, "4bf92f3577b34da6a3ce929d0e0e4736",
			map[int]string{0: "", 1: "4bf92f3577b34da6a3ce929d0e0e4736", 2: "", 3: "00112233445566778899aabbccddeeff"}},
	} {
		res := probe(t, ProbeRequest{
			Field: ProbeTraceID, Base: ProbeBase{Format: c.format}, Lines: c.lines,
			Marks: []ProbeMark{markAt(t, c.lines, c.mark, c.text)},
		})
		if res.Extractor == nil {
			t.Errorf("%s: no extractor: %s", name, res.Message)
			continue
		}
		for _, r := range res.Results {
			if r.Value != c.want[r.Line] {
				t.Errorf("%s: line %d = %q, want %q (%+v)", name, r.Line, r.Value, c.want[r.Line], res.Extractor)
			}
		}
		if res.Extractor.Field == "none" || res.Extractor.Pattern != "" {
			t.Errorf("%s: used a regex (%+v); position and framing text are enough here", name, res.Extractor)
		}
	}
}

// A spec's prefix and suffix are required: a field that lacks them has no
// value, so a value present on only some lines is not read from the others.
func TestExtractPrefixAndSuffixAreRequired(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", TraceIDField: "-1",
		Extract: map[string]ExtractSpec{"trace_id": {Prefix: "trace_id=", Suffix: ";"}},
	})
	for line, want := range map[string]string{
		"a trace_id=abc123;": "abc123",
		"a b 1ms":            "", // no prefix: not this value
		"a trace_id=abc123":  "", // no suffix: not this value
	} {
		if row, _ := p.ParseLine(line, t0); row.TraceId != want {
			t.Errorf("%q: trace = %q, want %q", line, row.TraceId, want)
		}
	}
}
