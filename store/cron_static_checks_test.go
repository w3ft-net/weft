package store

import (
	"strings"
	"testing"
)

// Two ways of writing a cron that quietly does the wrong thing are rejected
// when it is saved, so they never reach a run:
//   - an alert anywhere but the end of the body, which fires it a number of
//     times the author did not intend (once per loop turn, or on a branch);
//   - a weft:: function on the right of a pipe, which never reads what is
//     piped into it, so the upstream stage is silently discarded.

func TestCompileWatchPipeline_AlertMustBeTheFinalStage(t *testing.T) {
	bad := map[string]string{
		"alert then more work":       "echo 1 | alert above 0 --notify=ops\necho done",
		"alert in a loop":            "for h in a b; do echo 1 | alert above 0 --notify=ops; done",
		"alert in a conditional":     "if true; then echo 1 | alert above 0 --notify=ops; fi",
		"alert in an and-or list":    "true && echo 1 | alert above 0 --notify=ops",
		"alert in a group":           "{ echo 1 | alert above 0 --notify=ops; }",
		"alert in a subshell":        "(echo 1 | alert above 0 --notify=ops)",
		"two aliases on one alert":   "echo 1 | alert above 0 --notify=ops --notify=oncall",
		"alert before the last line": "echo 1 | alert above 0 --notify=ops\ncat /log/x/y/main.log | wc -l",
	}
	for name, body := range bad {
		if _, err := CompileWatchPipeline(body); err == nil {
			t.Errorf("%s: %q was accepted", name, body)
		}
	}

	good := map[string]string{
		"plain":            "cat /log/x/y/main.log | grep ERROR | wc -l | alert above 50 --notify=ops",
		"no alert":         "cat /log/x/y/main.log | wc -l",
		"composite":        "(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical",
		"per-tag":          "cat /log/x/y/disk.log | alert above 80 -v 2 -t 1 --notify=ops",
		"setup then alert": "x=5\necho $x | alert above 3 --notify=ops",
	}
	for name, body := range good {
		if _, err := CompileWatchPipeline(body); err != nil {
			t.Errorf("%s: %q rejected: %v", name, body, err)
		}
	}
}

func TestCompileWatchPipeline_WeftFunctionsDoNotReadAPipe(t *testing.T) {
	bad := map[string]string{
		"predicate after a pipe":      "echo 5 | weft::is_above error-rate 3",
		"aggregate after a pipe":      "cat /log/x/y/main.log | weft::avg error-rate 5m",
		"inside a subshell":           "(cat /log/x/y/main.log | weft::is_below disk 2 && echo fire) | alert --notify=ops",
		"third stage":                 "cat /log/x/y/main.log | wc -l | weft::is_absent canary-ping",
		"piped into a describe stage": "echo error-rate | weft::describe",
	}
	for name, body := range bad {
		_, err := CompileWatchPipeline(body)
		if err == nil || !strings.Contains(err.Error(), "weft::") || !strings.Contains(err.Error(), "is_gt") {
			t.Errorf("%s: %q: err = %v, want a rejection that points at weft::is_gt / weft::is_lt", name, body, err)
		}
	}

	good := map[string]string{
		"first stage":        "weft::avg error-rate 5m | weft::is_gt 50",
		"comparator lt":      "weft::avg error-rate 5m | weft::is_lt 5",
		"comparator midpipe": "weft::avg error-rate 5m | weft::is_gt 50 | alert --notify=ops",
		"predicate alone":    "(weft::is_above error-rate 50 && echo fire) | alert --notify=ops",
		"describe first":     "weft::describe error-rate | jq -r .stdout",
	}
	for name, body := range good {
		if _, err := CompileWatchPipeline(body); err != nil {
			t.Errorf("%s: %q rejected: %v", name, body, err)
		}
	}
}
