package setup

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/w3ft-net/weft/ingest"
)

func reviewFixture(t *testing.T) *reviewModel {
	t.Helper()
	lines := []string{"a trace_id=abc1 z", "b nothing", "c trace_id=def2 z"}
	ex := &ingest.ProbeExtractor{Field: "none", Pattern: `trace_id=(\w+)`}
	res, err := ingest.Probe(ingest.ProbeRequest{Field: ingest.ProbeTraceID, Base: ingest.ProbeBase{Format: "text"}, Lines: lines, Previous: ex})
	if err != nil {
		t.Fatal(err)
	}
	return newReview("trace id", lines, ex, res, nil)
}

func key(m *reviewModel, k string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
}

// The cursor starts on the first missed line, since that is what needs a decision.
func TestReviewStartsOnFirstMiss(t *testing.T) {
	m := reviewFixture(t)
	if m.cur != 1 {
		t.Fatalf("cursor on %d, want 1 (the unmatched line)", m.cur)
	}
	key(m, "m")
	if !m.done || m.result.Kind != ReviewFlagMissing || m.result.Line != 1 {
		t.Errorf("result = %+v, want missing on line 1", m.result)
	}
}

func TestReviewFlagWrongAcceptRestartSkip(t *testing.T) {
	for _, c := range []struct {
		keys []string
		want ReviewResult
	}{
		{[]string{"k", "x"}, ReviewResult{Kind: ReviewFlagWrong, Line: 0}},
		{[]string{"j", "x"}, ReviewResult{Kind: ReviewFlagWrong, Line: 2}},
		{[]string{"a"}, ReviewResult{Kind: ReviewAccept}},
		{[]string{"r"}, ReviewResult{Kind: ReviewRestart}},
		{[]string{"s"}, ReviewResult{Kind: ReviewSkip}},
		{[]string{"G", "m"}, ReviewResult{Kind: ReviewFlagMissing, Line: 2}},
	} {
		m := reviewFixture(t)
		for _, k := range c.keys {
			key(m, k)
		}
		if !m.done || m.result != c.want {
			t.Errorf("%v: %+v, want %+v", c.keys, m.result, c.want)
		}
	}
}

func TestReviewViewShowsMatchCountAndPattern(t *testing.T) {
	v := reviewFixture(t).View()
	for _, want := range []string{"matched 2 of 3", "trace_id=(\\w+)", "c trace_id="} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
}

// Keys typed faster than they are read arrive as one message; each still counts.
func TestReviewHandlesCoalescedKeys(t *testing.T) {
	m := reviewFixture(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Gm")})
	if !m.done || m.result != (ReviewResult{Kind: ReviewFlagMissing, Line: 2}) {
		t.Errorf("result = %+v, want missing on line 2", m.result)
	}
}

// A rule the model proposed says so, since the person did not build it.
func TestReviewViewSaysWhenTheModelProposedTheRule(t *testing.T) {
	m := reviewFixture(t)
	if strings.Contains(m.View(), "suggested by the model") {
		t.Fatal("a built rule must not claim to be the model's")
	}
	m.res.Source = "model"
	if !strings.Contains(m.View(), "suggested by the model") {
		t.Error("view does not say the model proposed this rule")
	}
}

func TestDescribeExtractorSaysWhatIsStripped(t *testing.T) {
	for _, c := range []struct {
		ex   ingest.ProbeExtractor
		want string
	}{
		{ingest.ProbeExtractor{Field: "3", Spec: &ingest.ExtractSpec{CutAt: "/"}}, `field 3, cut at "/"`},
		{ingest.ProbeExtractor{Field: "-1", Spec: &ingest.ExtractSpec{Prefix: "trace_id="}}, `last field, without the leading "trace_id="`},
		{ingest.ProbeExtractor{Field: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05", Spec: &ingest.ExtractSpec{Prefix: "[", Suffix: ":"}},
			`fields 1-2, without the leading "[" and the trailing ":", format 2006-01-02 15:04:05`},
		{ingest.ProbeExtractor{Field: "-2"}, "second-to-last field"},
	} {
		if got := describeExtractor(&c.ex); got != c.want {
			t.Errorf("describeExtractor = %q, want %q", got, c.want)
		}
	}
}
