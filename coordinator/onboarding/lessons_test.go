package onboarding

import "testing"

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
		{"shell-grep-awk", "tail /log/loom/prod/api/main.log | grep ERROR | awk '{print HOST, $1, $5}'", true},
		{"shell-trace-find", "tail /log/loom/prod/api/main.log | grep checkout | awk '{print TRACE_ID}'", true},
		{"shell-alert", "tail /log/loom/prod/api/main.log | grep ERROR | wc -l | alert above 0", true},
		{"shell-notify", "cat /etc/notify.yaml", true},
		{"shell-cron", "ls -l /cron.d/", true},
		{"shell-bin-bash", "ls /bin/", true},
		{"shell-bin-lua", "vim /bin/severity", true},
		{"shell-wit", "wit log /bin/scan -p", true},
		{"shell-weftfunc", "weft::avg error-rate 1h", true},
		{"shell-history", "history", true},
		{"shell-tmp", "echo hello > /tmp/note", true},
		{"shell-chart-pipe", "tail /log/loom/prod/api/main.log | grep ERROR | chart -x 1 --bucket 5m", true},
		{"shell-dashboard-yaml", "cat /etc/dashboards/prod.yaml", true},
		{"shell-dashboard", "dashboard prod", true},

		// shell-grep-awk's OLD $4 form no longer matches after the
		// field-shift fix (trace_id inserted as loom-producer's line
		// field 3 pushed everything right by one).
		{"shell-grep-awk", "tail /log/loom/prod/api/main.log | grep ERROR | awk '{print HOST, $1, $4}'", false},
		// shell-cron/shell-wit's OLD forms (pre -l / pre -p) no longer match.
		{"shell-cron", "ls /cron.d/", false},
		{"shell-wit", "wit log /bin/scan", false},

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
