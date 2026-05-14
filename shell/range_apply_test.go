package shell

import (
	"strings"
	"testing"
)

// TestApplyRangeLine_BareReturnsCurrent confirms that a bare `range` input
// returns the Shell's stored spec resolved against now, formatted plainly,
// with no mutation of s.Range.
func TestApplyRangeLine_BareReturnsCurrent(t *testing.T) {
	s := &Shell{Range: "1h.."}

	handled, out, err := s.applyRangeLine("range")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out, "[1h]") {
		t.Errorf("expected duration suffix in %q", out)
	}
	if s.Range != "1h.." {
		t.Errorf("bare `range` should not mutate s.Range; got %q", s.Range)
	}
}

// TestApplyRangeLine_SpecMutatesWindow confirms that `range <spec>` parses,
// stores the raw spec on s.Range, and returns the formatted resolved line.
// Locks in the shared behavior between ExecNonInteractive and rangeBuiltinFor
// — they must agree on what `range 30m..` means.
func TestApplyRangeLine_SpecMutatesWindow(t *testing.T) {
	s := &Shell{Range: "1d.."}
	handled, out, err := s.applyRangeLine("range 30m..")
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if out == "" {
		t.Error("expected formatted range output")
	}
	if s.Range != "30m.." {
		t.Errorf("spec should be stored verbatim on s.Range; got %q", s.Range)
	}
}

// TestApplyRangeLine_UnknownDoesntHandle verifies that non-range
// lines fall through. The caller (ExecNonInteractive) needs this to
// continue dispatching to FS builtins / query pipeline.
func TestApplyRangeLine_UnknownDoesntHandle(t *testing.T) {
	s := &Shell{Range: "1d.."}
	handled, _, _ := s.applyRangeLine("ls /log")
	if handled {
		t.Error("non-range line should not be handled")
	}
}

// TestApplyRangeLine_BadSpecReturnsError confirms that an invalid spec is
// reported as handled-with-error and does NOT mutate s.Range. Without the
// "leave s.Range alone on error" guarantee a typo would silently scrub the
// user's previous (working) window.
func TestApplyRangeLine_BadSpecReturnsError(t *testing.T) {
	s := &Shell{Range: "1d.."}
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
	if s.Range != "1d.." {
		t.Errorf("invalid spec must not mutate s.Range; got %q", s.Range)
	}
}
