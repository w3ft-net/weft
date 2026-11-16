package setup

import (
	"errors"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/ingest"
)

// scriptUI answers the tree's questions from a script, recording what it was
// asked so a test can assert the walk, not just the outcome.
type scriptUI struct {
	t          *testing.T
	confirm    []bool // answers to ConfirmCandidate then ExpectOnLines, in order asked
	picks      []PickResult
	reviews    []ReviewResult
	asked      []string
	notified   []string
	spans      bool
	spansAsked bool
	unit       string // answer to DurationUnit
	larger     bool   // answer to OfferLargerSample
	offered    []ingest.ProbeField
}

func (u *scriptUI) ConfirmCandidate(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult) (bool, error) {
	u.asked = append(u.asked, "candidate")
	return u.nextConfirm(), nil
}
func (u *scriptUI) ExpectOnLines(f ingest.ProbeField) (bool, error) {
	u.asked = append(u.asked, "expect")
	return u.nextConfirm(), nil
}
func (u *scriptUI) nextConfirm() bool {
	if len(u.confirm) == 0 {
		u.t.Fatal("unexpected confirm question")
	}
	a := u.confirm[0]
	u.confirm = u.confirm[1:]
	return a
}
func (u *scriptUI) PickValue(f ingest.ProbeField, lines []string, line int, hint string) (PickResult, error) {
	u.asked = append(u.asked, "pick")
	if len(u.picks) == 0 {
		u.t.Fatalf("unexpected pick on line %d", line)
	}
	p := u.picks[0]
	u.picks = u.picks[1:]
	if p.Kind == PickSelected && p.Line == -1 {
		p.Line = line
	}
	return p, nil
}
func (u *scriptUI) Review(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult, marks []ingest.ProbeMark) (ReviewResult, error) {
	u.asked = append(u.asked, "review")
	if len(u.reviews) == 0 {
		u.t.Fatal("unexpected review")
	}
	r := u.reviews[0]
	u.reviews = u.reviews[1:]
	return r, nil
}
func (u *scriptUI) OfferLargerSample(f ingest.ProbeField) (bool, error) {
	u.offered = append(u.offered, f)
	return u.larger, nil
}
func (u *scriptUI) Notify(msg string) { u.notified = append(u.notified, msg) }
func (u *scriptUI) DurationUnit(sample []string) (string, error) {
	u.asked = append(u.asked, "unit")
	return u.unit, nil
}
func (u *scriptUI) WantSpans() (bool, error) {
	u.spansAsked = true
	return u.spans, nil
}

// sel selects text within lines[line] (first occurrence).
func sel(lines []string, line int, text string) PickResult {
	i := strings.Index(lines[line], text)
	return PickResult{Kind: PickSelected, Line: line, Start: i, End: i + len(text)}
}

var treeLines = []string{
	"12:00:00 starting worker",
	"12:00:01 listening on :8080",
	"12:00:02 GET /api/items 200 trace_id=3f9a1c0de4b2a871",
	"12:00:03 GET /healthz 200",
	"12:00:04 GET /api/orders 500 trace_id=aa01bb02cc03dd04",
}

func TestExtractFieldAcceptsDetectedCandidate(t *testing.T) {
	ui := &scriptUI{t: t, confirm: []bool{true}}
	det := &ingest.ProbeExtractor{Field: "none", Pattern: `trace_id=(\w+)`}
	got, err := ExtractField(ui, LocalProber(treeLines), ingest.ProbeTraceID, treeLines, ingest.ProbeBase{Format: "text"}, []*ingest.ProbeExtractor{det})
	if err != nil || got != det {
		t.Fatalf("got %+v, %v; want the detected candidate", got, err)
	}
	if len(ui.asked) != 1 {
		t.Errorf("asked %v, want only the candidate question", ui.asked)
	}
}

func TestExtractFieldSkipsWhenNotExpected(t *testing.T) {
	ui := &scriptUI{t: t, confirm: []bool{false, false}}
	det := &ingest.ProbeExtractor{Field: "none", Pattern: `nope=(\w+)`}
	got, err := ExtractField(ui, LocalProber(treeLines), ingest.ProbeTraceID, treeLines, ingest.ProbeBase{Format: "text"}, []*ingest.ProbeExtractor{det})
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nothing", got, err)
	}
	if strings.Join(ui.asked, ",") != "candidate,expect" {
		t.Errorf("asked %v", ui.asked)
	}
}

// Reject the guess, say the value is expected, walk past lines that lack it,
// highlight it, and accept the result.
func TestExtractFieldWalksLinesThenHighlights(t *testing.T) {
	ui := &scriptUI{
		t:       t,
		confirm: []bool{false, true},
		picks: []PickResult{
			{Kind: PickNextLine}, {Kind: PickNextLine}, // lines 0, 1 lack it
			sel(treeLines, 2, "3f9a1c0de4b2a871"),
		},
		reviews: []ReviewResult{{Kind: ReviewAccept}},
	}
	det := &ingest.ProbeExtractor{Field: "9"}
	got, err := ExtractField(ui, LocalProber(treeLines), ingest.ProbeTraceID, treeLines, ingest.ProbeBase{Format: "text"}, []*ingest.ProbeExtractor{det})
	if err != nil || got == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.Pattern != "" || got.Spec == nil || got.Spec.Prefix != "trace_id=" {
		t.Errorf("extractor = %+v, want a field framed by the required prefix trace_id= (no regex)", got)
	}
	if strings.Join(ui.asked, ",") != "candidate,expect,pick,pick,pick,review" {
		t.Errorf("asked %v", ui.asked)
	}
}

// A line the rule missed is flagged, the user highlights its value there,
// and the rule is rebuilt to cover both.
func TestExtractFieldFlagMissingRebuildsRule(t *testing.T) {
	lines := []string{
		"INFO handled request_id=a1b2c3d4",
		"INFO handled req=ffeeddcc",
	}
	ui := &scriptUI{
		t:       t,
		confirm: []bool{true}, // expected; no detected candidate so no candidate question
		picks: []PickResult{
			sel(lines, 0, "a1b2c3d4"),
			sel(lines, 1, "ffeeddcc"), // the flagged-missing line
		},
		reviews: []ReviewResult{
			{Kind: ReviewFlagMissing, Line: 1},
			{Kind: ReviewAccept},
		},
	}
	got, err := ExtractField(ui, LocalProber(lines), ingest.ProbeTraceID, lines, ingest.ProbeBase{Format: "text"}, nil)
	if err != nil || got == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	res, _ := ingest.Probe(ingest.ProbeRequest{Field: ingest.ProbeTraceID, Base: ingest.ProbeBase{Format: "text"}, Lines: lines, Previous: got})
	if res.Matched != 2 {
		t.Errorf("final rule %+v matches %d/2 lines", got, res.Matched)
	}
}

// Flagging a wrong match narrows the rule.
func TestExtractFieldFlagWrongNarrowsRule(t *testing.T) {
	lines := []string{
		"worker started req [deadbeef] ok",
		"build finished [cafebabe] ok",
	}
	ui := &scriptUI{
		t:       t,
		confirm: []bool{true},
		picks:   []PickResult{sel(lines, 0, "deadbeef")},
		reviews: []ReviewResult{{Kind: ReviewFlagWrong, Line: 1}, {Kind: ReviewAccept}},
	}
	got, err := ExtractField(ui, LocalProber(lines), ingest.ProbeTraceID, lines, ingest.ProbeBase{Format: "text"}, nil)
	if err != nil || got == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	res, _ := ingest.Probe(ingest.ProbeRequest{Field: ingest.ProbeTraceID, Base: ingest.ProbeBase{Format: "text"}, Lines: lines, Previous: got})
	if res.Results[1].Matched || !res.Results[0].Matched {
		t.Errorf("rule %+v: line0=%v line1=%v, want only line 0", got, res.Results[0].Matched, res.Results[1].Matched)
	}
}

// Walking every line without finding the value ends the field and says why,
// pointing at a resample.
func TestExtractFieldValueNotInSample(t *testing.T) {
	lines := []string{"a", "b"}
	ui := &scriptUI{
		t:       t,
		confirm: []bool{true},
		picks:   []PickResult{{Kind: PickNextLine}, {Kind: PickNextLine}},
	}
	got, err := ExtractField(ui, LocalProber(lines), ingest.ProbeTraceID, lines, ingest.ProbeBase{Format: "text"}, nil)
	if !errors.Is(err, ErrValueNotInSample) || got != nil {
		t.Fatalf("got %+v, %v; want ErrValueNotInSample", got, err)
	}
}

func TestExtractFieldRestartAndSkip(t *testing.T) {
	ui := &scriptUI{
		t:       t,
		confirm: []bool{true},
		picks:   []PickResult{sel(treeLines, 2, "3f9a1c0de4b2a871"), {Kind: PickSkip}},
		reviews: []ReviewResult{{Kind: ReviewRestart}},
	}
	got, err := ExtractField(ui, LocalProber(treeLines), ingest.ProbeTraceID, treeLines, ingest.ProbeBase{Format: "text"}, nil)
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want skip", got, err)
	}
}

// ExtractAll walks every value in order, starts each from detection's guess,
// reports a value that is not in the sample, and gates the span fields behind
// one question unless detection found some.
func TestExtractAllFillsSchemaAndGatesSpans(t *testing.T) {
	lines := []string{
		"2026-10-15 12:00:01 INFO a trace_id=abc123",
		"2026-10-15 12:00:02 WARN b",
		"2026-10-15 12:00:03 INFO c trace_id=def456",
	}
	det := &ingest.Detection{
		TimestampCandidates: []ingest.TimestampCandidate{{FieldIndex: 1, SpanFields: 2, Format: "2006-01-02 15:04:05"}},
		LevelCandidates:     []ingest.FieldCandidate{{FieldIndex: 3}},
	}
	ui := &scriptUI{
		t: t,
		// timestamp candidate yes, level candidate yes,
		// trace: no candidate -> expect yes
		// spans: not wanted
		confirm: []bool{true, true, true},
		picks:   []PickResult{sel(lines, 0, "abc123")},
		reviews: []ReviewResult{{Kind: ReviewAccept}},
		spans:   false,
	}
	schema := &ingest.Schema{Format: "text"}
	if err := ExtractAll(ui, LocalProber(lines), schema, lines, det); err != nil {
		t.Fatal(err)
	}
	if schema.TimestampField != "1" || schema.TimestampFields != 2 || schema.TimestampFormat != "2006-01-02 15:04:05" {
		t.Errorf("timestamp not applied: %+v", schema)
	}
	if schema.LevelField != "3" {
		t.Errorf("level field = %q, want 3", schema.LevelField)
	}
	if schema.TraceIDField != "5" || schema.TraceIDPattern != "" || schema.Extract["trace_id"].Prefix != "trace_id=" {
		t.Errorf("trace = field %q pattern %q spec %+v, want field 5 framed by the required prefix trace_id=",
			schema.TraceIDField, schema.TraceIDPattern, schema.Extract["trace_id"])
	}
	if schema.SpanIDField != "" || schema.SpanNameField != "" {
		t.Errorf("spans were declined but set: %+v", schema)
	}
	if !ui.spansAsked {
		t.Error("span gate not asked")
	}
}

// A value on none of the sampled lines offers a larger sample; accepting ends
// the walk so the caller can request it.
func TestExtractAllOffersLargerSampleWhenValueNotInSample(t *testing.T) {
	lines := []string{"a", "b"}
	script := func(larger bool) *scriptUI {
		return &scriptUI{
			t: t,
			// timestamp: expected? no. level: expected? no. trace: expected? yes,
			// then every line is shown without it. Spans: declined.
			confirm: []bool{false, false, true},
			picks:   []PickResult{{Kind: PickNextLine}, {Kind: PickNextLine}},
			larger:  larger,
		}
	}

	ui := script(true)
	err := ExtractAll(ui, LocalProber(lines), &ingest.Schema{Format: "text"}, lines, nil)
	if !errors.Is(err, ErrLargerSample) {
		t.Fatalf("err = %v, want ErrLargerSample", err)
	}
	if len(ui.offered) != 1 || ui.offered[0] != ingest.ProbeTraceID {
		t.Errorf("offered for %v, want only the trace ID", ui.offered)
	}

	ui = script(false)
	schema := &ingest.Schema{Format: "text"}
	if err := ExtractAll(ui, LocalProber(lines), schema, lines, nil); err != nil {
		t.Fatalf("declining must carry on, got %v", err)
	}
	if schema.TraceIDField != "" {
		t.Errorf("trace id set: %q", schema.TraceIDField)
	}
	if !ui.spansAsked {
		t.Error("declining should continue to the span gate")
	}
}

// What the wizard learns about how to read a value (what surrounds it, where
// to cut) lands in the schema's extract map; skipping the value removes it.
func TestApplyExtractorRecordsAndClearsSpec(t *testing.T) {
	s := &ingest.Schema{}
	applyExtractor(s, ingest.ProbeLevel, &ingest.ProbeExtractor{Field: "3", Spec: &ingest.ExtractSpec{CutAt: "/"}})
	if s.LevelField != "3" || s.Extract["level"].CutAt != "/" {
		t.Fatalf("schema = %+v", s)
	}
	applyExtractor(s, ingest.ProbeTraceID, &ingest.ProbeExtractor{Field: "none", Pattern: `id=(\w+)`})
	if _, ok := s.Extract["trace_id"]; ok {
		t.Error("a whole-line regex has no spec to record")
	}
	applyExtractor(s, ingest.ProbeLevel, nil)
	if s.LevelField != "" || len(s.Extract) != 0 {
		t.Errorf("skipping must clear the value and its spec: %+v", s)
	}
}

func TestExtractAllStoresDerivedSpec(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] Task a received",
		"[2026-10-15 12:00:02,456: WARNING/ForkPoolWorker-1] Task b slow",
	}
	ui := &scriptUI{
		t:       t,
		confirm: []bool{false, true, false}, // timestamp: not expected; level: expected; trace: not expected
		picks:   []PickResult{sel(lines, 0, "INFO")},
		reviews: []ReviewResult{{Kind: ReviewAccept}},
	}
	schema := &ingest.Schema{Format: "text"}
	if err := ExtractAll(ui, LocalProber(lines), schema, lines, nil); err != nil {
		t.Fatal(err)
	}
	if schema.LevelField != "3" || schema.Extract["level"].CutAt != "/" {
		t.Errorf("level not recorded as field 3 cutting at /: %+v %+v", schema.LevelField, schema.Extract)
	}
}

// The summary describes what the schema now holds, not what was said along
// the way: reading an extractor back out of a schema recovers it.
func TestExtractorFromSchemaRoundTrips(t *testing.T) {
	s := &ingest.Schema{}
	want := &ingest.ProbeExtractor{Field: "-1", Pattern: "", Spec: &ingest.ExtractSpec{Prefix: "trace_id="}}
	applyExtractor(s, ingest.ProbeTraceID, want)
	got := ingest.ExtractorFromSchema(s, ingest.ProbeTraceID)
	if got == nil || got.Field != "-1" || got.Spec == nil || got.Spec.Prefix != "trace_id=" {
		t.Errorf("round trip = %+v", got)
	}
	ts := &ingest.ProbeExtractor{Field: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05", Spec: &ingest.ExtractSpec{Prefix: "["}}
	applyExtractor(s, ingest.ProbeTimestamp, ts)
	if got := ingest.ExtractorFromSchema(s, ingest.ProbeTimestamp); got == nil || got.TimestampFields != 2 || got.TimestampFormat != ts.TimestampFormat {
		t.Errorf("timestamp round trip = %+v", got)
	}
	if ingest.ExtractorFromSchema(s, ingest.ProbeSpanID) != nil {
		t.Error("an unset value must read back as nothing")
	}
}

// Every detected candidate is offered in turn: rejecting the first shows the
// second, and only when all are rejected does the tree ask whether the value
// is expected at all.
func TestExtractFieldOffersEachCandidate(t *testing.T) {
	lines := []string{"GET INFO a", "POST WARN b", "GET INFO c", "POST ERROR d"}
	method := &ingest.ProbeExtractor{Field: "1", Spec: &ingest.ExtractSpec{}}
	level := &ingest.ProbeExtractor{Field: "2", Spec: &ingest.ExtractSpec{}}
	ui := &scriptUI{t: t, confirm: []bool{false, true}} // not the method; yes, the level
	got, err := ExtractField(ui, LocalProber(lines), ingest.ProbeLevel, lines, ingest.ProbeBase{Format: "text"}, []*ingest.ProbeExtractor{method, level})
	if err != nil || got != level {
		t.Fatalf("got %+v, %v; want the second candidate", got, err)
	}
	if strings.Join(ui.asked, ",") != "candidate,candidate" {
		t.Errorf("asked %v, want two candidate questions and nothing else", ui.asked)
	}

	// Both rejected: now the question about expecting it at all.
	ui = &scriptUI{t: t, confirm: []bool{false, false, false}}
	got, err = ExtractField(ui, LocalProber(lines), ingest.ProbeLevel, lines, ingest.ProbeBase{Format: "text"}, []*ingest.ProbeExtractor{method, level})
	if err != nil || got != nil || strings.Join(ui.asked, ",") != "candidate,candidate,expect" {
		t.Errorf("got %+v, %v asked %v", got, err, ui.asked)
	}
}

// A candidate is judged by what it reads, so the wizard lists the distinct
// values it sees (in order of first appearance, capped).
func TestDistinctValues(t *testing.T) {
	res := &ingest.ProbeResult{Results: []ingest.ProbeLineResult{
		{Matched: true, Value: "INFO"}, {Matched: true, Value: "WARN"}, {Matched: false},
		{Matched: true, Value: "INFO"}, {Matched: true, Value: "ERROR"},
	}}
	if got := distinctValues(res, 8); strings.Join(got, ",") != "INFO,WARN,ERROR" {
		t.Errorf("distinctValues = %v", got)
	}
	if got := distinctValues(res, 2); len(got) != 2 {
		t.Errorf("cap ignored: %v", got)
	}
}

// With nothing detected, the model's first guess is shown for confirmation
// before the person is asked to point at anything.
func TestExtractFieldOffersModelFirstGuess(t *testing.T) {
	guess := &ingest.ProbeExtractor{Field: "none", Pattern: `trace_id=(\S+)`}
	local := LocalProber(treeLines)
	proposeCalls := 0
	probe := func(req ingest.ProbeRequest) (*ingest.ProbeResult, error) {
		if req.Propose {
			proposeCalls++
			req.Previous, req.Propose = guess, false
		}
		return local(req)
	}
	ui := &scriptUI{t: t, confirm: []bool{true}}
	ex, err := ExtractField(ui, probe, ingest.ProbeTraceID, treeLines, ingest.ProbeBase{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ex == nil || ex.Pattern != guess.Pattern {
		t.Fatalf("extractor = %+v, want the model's guess", ex)
	}
	if proposeCalls != 1 || strings.Join(ui.asked, ",") != "candidate" {
		t.Errorf("propose calls=%d asked=%v, want 1 call and a single confirmation", proposeCalls, ui.asked)
	}
}

// A guess that was already shown (and turned down) as a detected candidate is
// not offered a second time.
func TestExtractFieldDoesNotRepeatRejectedGuess(t *testing.T) {
	cand := &ingest.ProbeExtractor{Field: "none", Pattern: `trace_id=(\S+)`}
	local := LocalProber(treeLines)
	probe := func(req ingest.ProbeRequest) (*ingest.ProbeResult, error) {
		if req.Propose {
			req.Previous, req.Propose = cand, false
		}
		return local(req)
	}
	ui := &scriptUI{t: t, confirm: []bool{false, false}} // reject the candidate, then "expect it?" no
	if _, err := ExtractField(ui, probe, ingest.ProbeTraceID, treeLines, ingest.ProbeBase{}, []*ingest.ProbeExtractor{cand}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ui.asked, ","); got != "candidate,expect" {
		t.Errorf("asked %s, want candidate,expect", got)
	}
}

// Marked duration text with no unit of its own prompts for the unit, and the
// answer decides what the number means.
func TestExtractFieldAsksUnitForBareDuration(t *testing.T) {
	lines := []string{
		"12:00:00 starting worker",
		"12:00:02 GET /api/items 200 took=12.5",
		"12:00:03 GET /healthz 200",
		"12:00:04 GET /api/orders 500 took=340",
	}
	ui := &scriptUI{t: t, confirm: []bool{true}, unit: "ms",
		picks:   []PickResult{{Kind: PickNextLine}, sel(lines, 1, "12.5")},
		reviews: []ReviewResult{{Kind: ReviewAccept}}}
	ex, err := ExtractField(ui, LocalProber(lines), ingest.ProbeDurationNs, lines, ingest.ProbeBase{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ex == nil || ex.DurationUnit != "ms" {
		t.Fatalf("extractor = %+v, want unit ms", ex)
	}
	if !strings.Contains(strings.Join(ui.asked, ","), "unit") {
		t.Errorf("unit never asked: %v", ui.asked)
	}
}

// Text that carries its own unit needs no question.
func TestExtractFieldDoesNotAskUnitForSuffixedDuration(t *testing.T) {
	lines := []string{
		"12:00:00 starting worker",
		"12:00:02 GET /api/items 200 took=12.5ms",
		"12:00:03 GET /healthz 200",
		"12:00:04 GET /api/orders 500 took=1.2s",
	}
	ui := &scriptUI{t: t, confirm: []bool{true},
		picks:   []PickResult{{Kind: PickNextLine}, sel(lines, 1, "12.5ms")},
		reviews: []ReviewResult{{Kind: ReviewAccept}}}
	ex, err := ExtractField(ui, LocalProber(lines), ingest.ProbeDurationNs, lines, ingest.ProbeBase{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ex == nil || ex.DurationUnit != "text" {
		t.Fatalf("extractor = %+v, want unit text", ex)
	}
	for _, a := range ui.asked {
		if a == "unit" {
			t.Errorf("asked for a unit although the text has one: %v", ui.asked)
		}
	}
}
