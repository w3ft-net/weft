package onboarding

import (
	"strings"
	"testing"
)

func TestCheckPipeline(t *testing.T) {
	cases := []struct {
		step     string
		pipeline string
		want     bool
	}{
		// Exact match advances.
		{"shell-ls", "ls /log", true},
		{"shell-tail", "tail /log/loom/prod/api/main.log", true},
		{"shell-tail-f", "tail -f /log/loom/prod/api/main.log", true},
		{"shell-cd", "cd /log/loom/prod/api", true},
		{"shell-grep-awk", "tail main.log | grep ERROR | awk '{print HOST, $1, $5}'", true},
		{"shell-trace-find", "tail main.log | grep ' checkout ' | awk '{print TRACE_ID}'", true},
		{"shell-alert", "tail main.log | grep ERROR | wc -l | alert above 0", true},
		{"shell-notify", "cat /etc/notify.yaml", true},
		{"shell-cron", "ls -l /cron.d/", true},
		{"shell-bin-bash", "ls /bin/", true},
		{"shell-bin-lua", "vim /bin/severity", true},
		{"shell-wit", "git log -p -- /bin/scan", true},
		{"shell-weftfunc", "weft::avg error-rate 1h", true},
		{"shell-history", "history", true},
		{"shell-tmp", "echo hello > /tmp/note", true},
		{"shell-chart-pipe", "cat main.log | grep ERROR | chart -x 1 --bucket 5m", true},
		{"shell-dashboard-yaml", "cat /etc/dashboards/prod.yaml", true},
		{"shell-dashboard", "dashboard prod", true},

		// shell-grep-awk's OLD $4 form no longer matches after the
		// field-shift fix (trace_id inserted as loom-producer's line
		// field 3 pushed everything right by one).
		{"shell-grep-awk", "tail main.log | grep ERROR | awk '{print HOST, $1, $4}'", false},
		// shell-cron/shell-wit's OLD forms (pre -l / pre -p) no longer match.
		{"shell-cron", "ls /cron.d/", false},
		{"shell-wit", "git log -- /bin/scan", false},

		// shell-trace is pattern-matched, not exact-match: a real
		// 32-lowercase-hex trace ID advances even though it's never
		// literally equal to Action (which is an illustrative
		// placeholder, not something any two users would share).
		{"shell-trace", "trace 0123456789abcdef0123456789abcdef", true},
		{"shell-trace", "  trace 0123456789abcdef0123456789abcdef  ", true},
		// The placeholder itself — re-pasted unedited — must NOT match;
		// that would let a user "succeed" without ever substituting a
		// real discovered ID.
		{"shell-trace", "trace <paste the id you copied>", false},
		// Malformed IDs: wrong length, uppercase, non-hex, missing arg.
		{"shell-trace", "trace 0123456789abcdef", false},
		{"shell-trace", "trace 0123456789ABCDEF0123456789ABCDEF", false},
		{"shell-trace", "trace 0123456789abcdef0123456789abcdeg", false},
		{"shell-trace", "trace", false},

		// Surrounding whitespace is forgiven.
		{"shell-ls", "  ls /log  ", true},
		{"shell-ls", "ls /log\n", true},

		// Edits don't advance — the user has wandered off and
		// should re-paste from the tour overlay.
		{"shell-ls", "ls -la /log", false},
		{"shell-ls", "ls /log/", false},
		{"shell-tail", "tail -n 50 /log/loom/prod/api/main.log", false},
		{"shell-ls", "cat /log/foo.log", false},
		{"shell-grep-awk", "cat /log/loom/prod/api/main.log | grep ERROR", false},

		// Unknown step / empty step.
		{"shell-unknown", "ls /log", false},
		{"", "ls /log", false},
	}
	for _, tc := range cases {
		got := CheckPipeline(tc.step, tc.pipeline)
		if got != tc.want {
			t.Errorf("CheckPipeline(%q, %q) = %v, want %v", tc.step, tc.pipeline, got, tc.want)
		}
	}
}

// TestStepsAreUnique guards against a duplicate Step key silently
// making Lookup return the wrong lesson for one of the two steps that
// share it.
func TestStepsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, step := range Steps() {
		if seen[step] {
			t.Errorf("duplicate lesson Step %q", step)
		}
		seen[step] = true
	}
}

// TestEveryLessonHasActionOrPattern guards against a lesson that
// would make CheckPipeline unconditionally false (empty Action and no
// ActionPattern) — a user could never advance past it.
func TestEveryLessonHasActionOrPattern(t *testing.T) {
	for _, step := range Steps() {
		l := Lookup(step)
		if l.Action == "" && l.ActionPattern == nil {
			t.Errorf("lesson %q has neither Action nor ActionPattern — no pipeline could ever advance it", step)
		}
	}
}

// TourOverlay renders the shell-phase card only for steps whose name starts
// with "shell-" (any other non-panel step makes the overlay unmount, leaving
// the user with no card and no prefilled command). Every lesson must keep
// that prefix.
func TestEveryLessonStepHasShellPrefix(t *testing.T) {
	for _, step := range Steps() {
		if !strings.HasPrefix(step, "shell-") {
			t.Errorf("lesson step %q must start with \"shell-\" or the tour overlay disappears at that lesson", step)
		}
	}
}

// A lesson advances on a clean exit. Ctrl+C reports exit 130 (SIGINT), which
// normally means "interrupted before it finished" — except for the live-tail
// lesson, where stopping the stream is exactly how the lesson is completed.
func TestAdvancesOnExit(t *testing.T) {
	cases := []struct {
		step string
		exit int
		want bool
	}{
		{"shell-history", 0, true},
		{"shell-history", 1, false},
		{"shell-history", 130, false}, // interrupting an ordinary command is not completing it
		{"shell-tail-f", 0, true},
		{"shell-tail-f", 130, true}, // stopping the live tail IS the lesson
		{"shell-tail-f", 1, false},
		{"no-such-step", 0, false},
	}
	for _, c := range cases {
		if got := AdvancesOnExit(c.step, c.exit); got != c.want {
			t.Errorf("AdvancesOnExit(%q, %d) = %v, want %v", c.step, c.exit, got, c.want)
		}
	}
}

// shell-cd teaches that after cd-ing into a service directory the long
// /log/... prefix can be dropped. Every later lesson that reads a log must
// then use the short form, or it undercuts the lesson it just taught. Other
// mounts (/etc, /cron.d, /bin, /tmp) are unaffected.
func TestLessonsAfterCdUseRelativeLogPaths(t *testing.T) {
	afterCd := false
	for _, step := range Steps() {
		if step == "shell-cd" {
			afterCd = true
			continue
		}
		if !afterCd {
			continue
		}
		if a := Lookup(step).Action; strings.Contains(a, "/log/") {
			t.Errorf("lesson %q follows shell-cd but its Action uses an absolute log path: %q", step, a)
		}
	}
}

// A lesson's prompt tells the user what to run; it must name the command the
// prefilled Action actually runs.
func TestShellLsLoomPromptMatchesAction(t *testing.T) {
	l := Lookup("shell-ls-loom")
	if !strings.Contains(l.Prompt, "`tree`") {
		t.Errorf("shell-ls-loom Action is %q but the prompt never mentions `tree`: %q", l.Action, l.Prompt)
	}
}

// A lesson whose command opens a modal (trace) completes when the browser
// reports the modal closed, or — for clients with no modal, like the CLI —
// when the server sees the pipeline finish. A browser must not complete it on
// the command finishing or on the modal's own row fetch, or the next prefill
// lands underneath the open modal. Every other lesson completes with its
// command and ignores modal-close reports.
func TestCompletesOn(t *testing.T) {
	cases := []struct {
		step    string
		trigger Trigger
		want    bool
	}{
		{"shell-chart", TriggerModalClosed, true},
		{"shell-chart", TriggerBrowserDone, false},
		{"shell-chart-pipe", TriggerModalClosed, true},
		{"shell-chart-pipe", TriggerBrowserDone, false},
		{"shell-trace", TriggerModalClosed, true},
		{"shell-trace", TriggerExec, true},
		{"shell-trace", TriggerBrowserDone, false},
		{"shell-trace", TriggerModalFetch, false},
		{"shell-history", TriggerExec, true},
		{"shell-history", TriggerBrowserDone, true},
		{"shell-history", TriggerModalFetch, true},
		{"shell-history", TriggerModalClosed, false},
		{"no-such-step", TriggerExec, false},
	}
	for _, c := range cases {
		if got := CompletesOn(c.step, c.trigger); got != c.want {
			t.Errorf("CompletesOn(%q, %v) = %v, want %v", c.step, c.trigger, got, c.want)
		}
	}
}
