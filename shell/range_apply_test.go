package shell

import (
	"strings"
	"testing"
	"time"
)

// TestApplyRangeLine_BareReturnsCurrent confirms that a bare `range`
// input returns the Shell's stored window formatted plainly, with no
// mutation.
func TestApplyRangeLine_BareReturnsCurrent(t *testing.T) {
	start := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	s := &Shell{Start: start, End: end}

	handled, out, err := s.applyRangeLine("range")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out, "[1h]") {
		t.Errorf("expected duration suffix in %q", out)
	}
	if !s.Start.Equal(start) || !s.End.Equal(end) {
		t.Errorf("bare `range` should not mutate window; got %v..%v", s.Start, s.End)
	}
}

// TestApplyRangeLine_SpecMutatesWindow confirms that `range <spec>`
// parses, mutates the Shell window, and returns the new formatted
// line. Locks in the shared behavior between ExecNonInteractive and
// rangeBuiltin — they must agree on what `range 30m..` means.
func TestApplyRangeLine_SpecMutatesWindow(t *testing.T) {
	s := &Shell{}
	handled, out, err := s.applyRangeLine("range 30m..")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if out == "" {
		t.Error("expected formatted range output")
	}
	if s.Start.IsZero() || s.End.IsZero() {
		t.Errorf("spec should set Start/End; got start=%v end=%v", s.Start, s.End)
	}
	if dur := s.End.Sub(s.Start); dur < 29*time.Minute || dur > 31*time.Minute {
		t.Errorf("expected ~30m window, got %v", dur)
	}
}

// TestApplyRangeLine_UnknownDoesntHandle verifies that non-range
// lines fall through. The caller (ExecNonInteractive) needs this to
// continue dispatching to FS builtins / query pipeline.
func TestApplyRangeLine_UnknownDoesntHandle(t *testing.T) {
	s := &Shell{}
	handled, _, _ := s.applyRangeLine("ls /log")
	if handled {
		t.Error("non-range line should not be handled")
	}
}

// TestApplyRangeLine_BadSpecReturnsError confirms that an invalid
// spec is reported as handled-with-error rather than swallowed.
func TestApplyRangeLine_BadSpecReturnsError(t *testing.T) {
	s := &Shell{}
	handled, out, err := s.applyRangeLine("range not-a-time")
	if !handled {
		t.Fatal("bad spec should still be handled (it's a range command)")
	}
	if err == nil {
		t.Error("bad spec should return an error")
	}
	if out != "" {
		t.Errorf("error path should produce no output; got %q", out)
	}
}
