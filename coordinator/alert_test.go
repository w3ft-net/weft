package coordinator

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/alert"
)

func runAlert(args string, stdin string) (stdout, stderr string, exitCode int) {
	var outBuf, errBuf bytes.Buffer
	ctx := &AlertContext{Interactive: true}
	code := AlertCommand(context.Background(), strings.Fields(args), strings.NewReader(stdin), &outBuf, &errBuf, ctx)
	return outBuf.String(), errBuf.String(), code
}

func TestAlert_BareFiresOnNonEmpty(t *testing.T) {
	// `echo x | alert` with no condition and no --notify: default "any".
	_, stderr, code := runAlert("", "something happened\n")
	if code != 0 || strings.Contains(stderr, "usage:") {
		t.Errorf("bare alert: code=%d stderr=%q, want a fire, not a usage error", code, stderr)
	}
}

func TestAlert_AnyNonEmpty(t *testing.T) {
	// Default condition fires on any non-empty stdin.
	_, stderr, code := runAlert("--notify=ops", "something happened\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr == "" {
		t.Error("expected alert for non-empty stdin")
	}
}

func TestAlert_AnyEmpty(t *testing.T) {
	// No stdin means no trigger.
	_, stderr, code := runAlert("--notify=ops", "")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("expected no alert for empty stdin, got %q", stderr)
	}
}

func TestAlert_MultiRow_JSONPath_FiresPerRow(t *testing.T) {
	// JSONL stdin (e.g. produced by jq); -v and -t are JSON paths.
	stdin := `{"host":"shard-a","disk":85}
{"host":"shard-b","disk":90}
{"host":"shard-c","disk":50}
`
	_, stderr, code := runAlert("above 80 -v .disk -t .host", stdin)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "shard-a") {
		t.Errorf("stderr missing shard-a alert: %q", stderr)
	}
	if !strings.Contains(stderr, "shard-b") {
		t.Errorf("stderr missing shard-b alert: %q", stderr)
	}
	if strings.Contains(stderr, "shard-c") {
		t.Errorf("shard-c (50) should not have fired: %q", stderr)
	}
}

func TestAlert_MultiRow_FiresPerRow(t *testing.T) {
	// Three hosts; only the first two are above 80. Both should fire, each
	// with its own tag prefix in the stderr line.
	stdin := "shard-a 85\nshard-b 90\nshard-c 50\n"
	_, stderr, code := runAlert("above 80 -v 2 -t 1", stdin)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "shard-a") {
		t.Errorf("stderr missing shard-a alert: %q", stderr)
	}
	if !strings.Contains(stderr, "shard-b") {
		t.Errorf("stderr missing shard-b alert: %q", stderr)
	}
	if strings.Contains(stderr, "shard-c") {
		t.Errorf("shard-c (50) should not have fired: %q", stderr)
	}
}

func TestAlert_MultiRow_BelowThresholdSilent(t *testing.T) {
	stdin := "shard-a 50\nshard-b 30\n"
	_, stderr, code := runAlert("above 80 -v 2 -t 1", stdin)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr != "" {
		t.Errorf("no rows should fire; got %q", stderr)
	}
}

func TestAlert_MultiRow_SkipsBlankLines(t *testing.T) {
	stdin := "\nshard-a 85\n\nshard-b 60\n\n"
	_, stderr, _ := runAlert("above 80 -v 2 -t 1", stdin)
	// Only shard-a should fire.
	if !strings.Contains(stderr, "shard-a") {
		t.Errorf("expected shard-a alert, got %q", stderr)
	}
	if strings.Contains(stderr, "shard-b") {
		t.Errorf("shard-b (60) should not have fired: %q", stderr)
	}
}

func TestAlert_MultiRow_NonNumericFieldSkipped(t *testing.T) {
	// Row 2 has a non-numeric "oops" in the value field. We still process
	// the other rows; the bad one produces a stderr line and exit 1.
	stdin := "shard-a 85\nshard-b oops\nshard-c 92\n"
	_, stderr, code := runAlert("above 80 -v 2 -t 1", stdin)
	if code != 1 {
		t.Errorf("expected exit 1 from non-numeric row, got %d", code)
	}
	if !strings.Contains(stderr, "non-numeric") {
		t.Errorf("expected non-numeric warning, got %q", stderr)
	}
	if !strings.Contains(stderr, "shard-a") || !strings.Contains(stderr, "shard-c") {
		t.Errorf("good rows should still fire; got %q", stderr)
	}
}

func TestAlert_NotifyFlagForms(t *testing.T) {
	// Both --notify=x and --notify x should parse the alias.
	for _, args := range []string{"--notify=ops", "--notify ops"} {
		parsed, err := alert.ParseArgs(strings.Fields(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if parsed.Notify != "ops" {
			t.Errorf("%s: got notify=%q", args, parsed.Notify)
		}
	}
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

func TestAlert_ParseArgsRejectsUnknownOptions(t *testing.T) {
	for _, args := range []string{"--foo", "above 50 --foo", "above 50 -x", "above 50 --msg hi --severity high", "-z"} {
		_, err := alert.ParseArgs(strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), "unknown option") {
			t.Errorf("alert.ParseArgs(%q) = %v, want unknown option error", args, err)
		}
	}
}

func TestAlert_ParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args string
		ok   bool
	}{
		{"bare alert defaults to any", "", true},
		{"above", "above 50", true},
		{"below", "below 10", true},
		{"eq", "eq 0", true},
		{"neq", "neq 0", true},
		{"changed", "changed", true},
		{"absent", "absent", true},
		{"rising", "rising 100%", true},
		{"falling", "falling 50%", true},
		{"matches", "matches ^old-", true},
		{"matches missing pattern", "matches", false},
		{"matches bad pattern", "matches (", false},
		{"above with duration", "above 50 --duration 5m", true},
		{"above with average", "above 50 --duration 10m --average", true},
		{"above with msg", "above 50 --msg alert!", true},
		{"unknown", "bogus 50", false},
		{"above missing threshold", "above", false},
		{"rising missing pct", "rising", false},
		{"-v positive", "above 1 -v 2", true},
		{"-v non-numeric", "above 1 -v abc", false},
		{"-v zero rejected", "above 1 -v 0", false},
		{"-v JSON path", "above 1 -v .latency_ms", true},
		{"-v nested JSON path", "above 1 -v .req.latency_ms", true},
		{"-v bare dot rejected", "above 1 -v .", false},
		{"-t positive", "above 1 -v 2 -t 1", true},
		{"--tag long form", "above 1 -v 2 --tag 3", true},
		{"--value long form", "above 1 --value 2", true},
		{"-t JSON path", "above 1 -v .v -t .host", true},
		{"mixing -v positional with -t JSON path rejected", "above 1 -v 2 -t .host", false},
		{"mixing -v JSON path with -t positional rejected", "above 1 -v .v -t 1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := alert.ParseArgs(strings.Fields(tt.args))
			if tt.ok && err != nil {
				t.Errorf("alert.ParseArgs(%q): unexpected error: %v", tt.args, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("alert.ParseArgs(%q): expected error", tt.args)
			}
		})
	}
}

// TestRunAlertStage exercises the integration point the query path relies on:
// a QueryResult with Text (from compute output) or Rows (from raw shard
// output) is fed into AlertCommand, and the alert's stderr lands on
// result.Stderr for the caller to render.
func TestRunAlertStage(t *testing.T) {
	t.Run("text above threshold", func(t *testing.T) {
		result := &QueryResult{Text: "73\n"}
		runAlertStage(context.Background(), result, []string{"above", "50"}, &AlertContext{Interactive: true})
		if result.Stderr == "" || !strings.Contains(result.Stderr, "73") {
			t.Errorf("expected 'above' alert in Stderr, got %q", result.Stderr)
		}
		if result.Text != "" {
			t.Errorf("interactive alert should suppress stdout, got %q", result.Text)
		}
	})

	t.Run("rows multi-row tagged", func(t *testing.T) {
		result := &QueryResult{Text: "shard-a 85\nshard-b 50\n"}
		runAlertStage(context.Background(), result, []string{"above", "80", "-v", "2", "-t", "1"},
			&AlertContext{Interactive: true})
		if !strings.Contains(result.Stderr, "shard-a") {
			t.Errorf("shard-a (85) should fire; stderr=%q", result.Stderr)
		}
		if strings.Contains(result.Stderr, "shard-b") {
			t.Errorf("shard-b (50) should not fire; stderr=%q", result.Stderr)
		}
	})

	t.Run("below threshold silent", func(t *testing.T) {
		result := &QueryResult{Text: "12\n"}
		runAlertStage(context.Background(), result, []string{"above", "50"}, &AlertContext{Interactive: true})
		if result.Stderr != "" {
			t.Errorf("below threshold should stay silent, got %q", result.Stderr)
		}
	})

	t.Run("surfaces non-numeric diagnostic", func(t *testing.T) {
		// Reproduces the UX problem the user hit: jq emits multiple numeric
		// lines but single-row alert treats them as one scalar. The error
		// message should make the mistake obvious.
		result := &QueryResult{Text: "5.12\n5.13\n"}
		runAlertStage(context.Background(), result, []string{"above", "5"}, &AlertContext{Interactive: true})
		if !strings.Contains(result.Stderr, "non-numeric") {
			t.Errorf("expected non-numeric diagnostic, got %q", result.Stderr)
		}
	})
}

// --if-zero / --if-nonzero exit-status forms. The cron composes
// predicates with bash &&/||, then `alert --if-zero "$?"` reads the
// expanded exit code directly — no `echo $? | alert below 1` plumbing.

func TestAlert_IfZero_ChainSucceeded(t *testing.T) {
	// "$?" expanded to "0" → chain succeeded → fire.
	_, stderr, code := runAlert("--if-zero 0 --notify=ops", "")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr, "if-zero") {
		t.Errorf("expected predicate-composite alert msg, got %q", stderr)
	}
}

func TestAlert_IfZero_ChainFailed(t *testing.T) {
	// "$?" expanded to "1" → chain failed → no fire.
	_, stderr, code := runAlert("--if-zero 1 --notify=ops", "")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if stderr != "" {
		t.Errorf("expected no alert for non-zero exit, got %q", stderr)
	}
}

func TestAlert_IfNonzero_ChainFailed(t *testing.T) {
	_, stderr, code := runAlert("--if-nonzero 1 --notify=ops", "")
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr, "if-nonzero") {
		t.Errorf("expected predicate-composite alert msg, got %q", stderr)
	}
}

func TestAlert_IfMode_RejectsConditionKeyword(t *testing.T) {
	parsed, err := alert.ParseArgs(strings.Fields("above 50 --if-zero 0"))
	if err == nil {
		t.Errorf("expected parse error mixing condition + --if-zero, got %+v", parsed)
	}
}

// TestAlert_Matches fires per tag on the rows whose value matches, and
// ignores the rest.
func TestAlert_Matches(t *testing.T) {
	stdin := "old-7 lease-a\nnew-2 lease-b\nold-9 lease-c\n"
	stdout, stderr, code := runAlert("matches ^old- -v 1 -t 2", stdin)
	if code != 0 || stdout != stdin {
		t.Fatalf("code=%d stdout=%q, want 0 and stdin passed through", code, stdout)
	}
	for _, want := range []string{`weft[lease-a]: "old-7" matches ^old-`, `weft[lease-c]: "old-9" matches ^old-`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q, want %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "lease-b") {
		t.Errorf("lease-b (new-2) should not have fired: %q", stderr)
	}
	if _, stderr, _ := runAlert("matches ^old-", ""); stderr != "" {
		t.Errorf("empty stdin fired: %q", stderr)
	}
}
