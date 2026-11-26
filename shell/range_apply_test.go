package shell

import (
	"bytes"
	"strings"
	"testing"
)

// A bare `range` prints the stored window resolved against now, with no
// mutation of s.Range.
func TestExecNonInteractive_BareRangeReturnsCurrent(t *testing.T) {
	s := &Shell{Range: "1h.."}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("range", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[1h]") {
		t.Errorf("expected duration suffix in %q", buf.String())
	}
	if s.Range != "1h.." {
		t.Errorf("bare `range` should not mutate s.Range; got %q", s.Range)
	}
}

// `range <spec>` stores the raw spec on s.Range and prints the resolved line.
func TestExecNonInteractive_RangeSpecMutatesWindow(t *testing.T) {
	s := &Shell{Range: "1d.."}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("range 30m..", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Error("expected formatted range output")
	}
	if s.Range != "30m.." {
		t.Errorf("spec should be stored verbatim on s.Range; got %q", s.Range)
	}
}

// A bad spec is an error and must not scrub the user's previous, working
// window.
func TestExecNonInteractive_BadRangeSpecReturnsError(t *testing.T) {
	s := &Shell{Range: "1d.."}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("range not-a-time", &buf); err == nil {
		t.Error("bad spec should return an error")
	}
	if buf.Len() != 0 {
		t.Errorf("error path should produce no output; got %q", buf.String())
	}
	if s.Range != "1d.." {
		t.Errorf("invalid spec must not mutate s.Range; got %q", s.Range)
	}
}

// `range` is the client's even in the middle of a line: the window it sets is
// the one the query after it ships with.
func TestExecNonInteractive_RangeThenQueryShipsWithTheNewWindow(t *testing.T) {
	var gotRange, gotLine string
	s := &Shell{Range: "1d..", Query: func(line, rng string, _ []string) (*QueryResult, error) {
		gotLine, gotRange = line, rng
		return &QueryResult{Text: "ok\n"}, nil
	}}
	var buf bytes.Buffer
	if err := s.ExecNonInteractive("range 5m..; cat /log/acme/api.log", &buf); err != nil {
		t.Fatal(err)
	}
	if gotLine != "cat /log/acme/api.log" || gotRange != "5m.." {
		t.Errorf("shipped %q with range %q", gotLine, gotRange)
	}
}
