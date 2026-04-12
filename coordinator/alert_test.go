package coordinator

import (
	"bytes"
	"strings"
	"testing"
)

func runAlert(args string, stdin string) (stdout, stderr string, exitCode int) {
	var outBuf, errBuf bytes.Buffer
	ctx := &AlertContext{Interactive: true}
	code := AlertCommand(strings.Fields(args), strings.NewReader(stdin), &outBuf, &errBuf, ctx)
	return outBuf.String(), errBuf.String(), code
}

func TestAlert_Above(t *testing.T) {
	stdout, stderr, code := runAlert("above 50", "73\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Tee: stdout should have the input.
	if !strings.Contains(stdout, "73") {
		t.Errorf("stdout should pass through input, got %q", stdout)
	}
	// 73 > 50: should alert with "weft:" prefix (interactive mode).
	if stderr == "" {
		t.Error("expected stderr alert for 73 > 50")
	}
	if !strings.Contains(stderr, "weft:") {
		t.Errorf("expected 'weft:' prefix in interactive mode, got %q", stderr)
	}
}

func TestAlert_AboveNoTrigger(t *testing.T) {
	stdout, stderr, code := runAlert("above 50", "3\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "3") {
		t.Errorf("stdout should pass through, got %q", stdout)
	}
	if stderr != "" {
		t.Errorf("should not alert for 3 < 50, got stderr %q", stderr)
	}
}

func TestAlert_Below(t *testing.T) {
	_, stderr, _ := runAlert("below 10", "3\n")
	if stderr == "" {
		t.Error("expected alert for 3 < 10")
	}
}

func TestAlert_BelowNoTrigger(t *testing.T) {
	_, stderr, _ := runAlert("below 10", "73\n")
	if stderr != "" {
		t.Errorf("should not alert for 73 > 10, got %q", stderr)
	}
}

func TestAlert_Eq(t *testing.T) {
	_, stderr, _ := runAlert("eq 42", "42\n")
	if stderr == "" {
		t.Error("expected alert for 42 == 42")
	}
}

func TestAlert_EqNoTrigger(t *testing.T) {
	_, stderr, _ := runAlert("eq 42", "43\n")
	if stderr != "" {
		t.Errorf("should not alert for 43 != 42, got %q", stderr)
	}
}

func TestAlert_Neq(t *testing.T) {
	_, stderr, _ := runAlert("neq 0", "5\n")
	if stderr == "" {
		t.Error("expected alert for 5 != 0")
	}
}

func TestAlert_NeqNoTrigger(t *testing.T) {
	_, stderr, _ := runAlert("neq 0", "0\n")
	if stderr != "" {
		t.Errorf("should not alert for 0 == 0, got %q", stderr)
	}
}

func TestAlert_TeePassthrough(t *testing.T) {
	// Regardless of trigger, stdout should always have the input.
	stdout1, _, _ := runAlert("above 50", "73\n")
	stdout2, _, _ := runAlert("above 50", "3\n")
	if !strings.Contains(stdout1, "73") {
		t.Errorf("triggered case: stdout = %q, want '73'", stdout1)
	}
	if !strings.Contains(stdout2, "3") {
		t.Errorf("non-triggered case: stdout = %q, want '3'", stdout2)
	}
}

func TestAlert_CustomMsg(t *testing.T) {
	_, stderr, _ := runAlert("above 0 --msg fire!", "5\n")
	if !strings.Contains(stderr, "fire!") {
		t.Errorf("expected custom message 'fire!', got %q", stderr)
	}
}

func TestAlert_NonNumeric(t *testing.T) {
	_, stderr, code := runAlert("above 50", "hello\n")
	if code != 1 {
		t.Errorf("expected exit code 1 for non-numeric, got %d", code)
	}
	if !strings.Contains(stderr, "non-numeric") {
		t.Errorf("expected non-numeric error, got %q", stderr)
	}
}

func TestAlert_BadArgs(t *testing.T) {
	_, _, code := runAlert("", "5\n")
	if code != 1 {
		t.Error("expected error for empty args")
	}
}

func TestAlert_Rising_NoPrevious(t *testing.T) {
	// No history in interactive mode — rising can't evaluate, should not trigger.
	_, stderr, code := runAlert("rising 100%", "50\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("rising with no history should not trigger, got %q", stderr)
	}
}

func TestAlert_ParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args string
		ok   bool
	}{
		{"above", "above 50", true},
		{"below", "below 10", true},
		{"eq", "eq 0", true},
		{"neq", "neq 0", true},
		{"changed", "changed", true},
		{"absent", "absent", true},
		{"rising", "rising 100%", true},
		{"falling", "falling 50%", true},
		{"above with duration", "above 50 --duration 5m", true},
		{"above with average", "above 50 --duration 10m --average", true},
		{"above with msg", "above 50 --msg alert!", true},
		{"unknown", "bogus 50", false},
		{"above missing threshold", "above", false},
		{"rising missing pct", "rising", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAlertArgs(strings.Fields(tt.args))
			if tt.ok && err != nil {
				t.Errorf("parseAlertArgs(%q): unexpected error: %v", tt.args, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("parseAlertArgs(%q): expected error", tt.args)
			}
		})
	}
}
