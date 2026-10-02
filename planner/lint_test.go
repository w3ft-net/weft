package planner

import "testing"

// lowerFor is a small helper that lowers pipeline with a resolver
// that knows the real /bin/log-pattern-count body, so tests exercise
// LintTruncationRisk against the exact shape a saved cron produces
// after save-time /bin inlining — not a hand-simplified stand-in.
func lowerFor(t *testing.T, pipeline string) *LoweredPlan {
	t.Helper()
	resolver := fakeBinResolver{binByName: map[string]string{
		"log-pattern-count": `tail -n $2 /log/weft/$1/main.log | grep -c "$3"`,
		"runtime-field":     `tail -n 1 /log/weft/$1/runtime.log | jq -r ".$2"`,
	}}
	lp, err := Lower(pipeline, 1, "bolt", resolver)
	if err != nil {
		t.Fatalf("Lower(%q): %v", pipeline, err)
	}
	return lp
}

func TestLintTruncationRisk(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		want     bool // true = expect a warning
	}{
		{
			name:     "raw tail piped into grep -c",
			pipeline: `tail -n 1000 /log/weft/coordinator/main.log | grep -c "stream error"`,
			want:     true,
		},
		{
			name:     "log-pattern-count bin script (real /bin body)",
			pipeline: `log-pattern-count coordinator/main 5000 'stream error'`,
			want:     true,
		},
		{
			name:     "with a trailing alert stage",
			pipeline: `log-pattern-count coordinator/main 1000 'no LAUNCHING row' | alert above 5 --notify=page`,
			want:     true,
		},
		{
			name:     "distinct-host count via uniq -c",
			pipeline: `tail -n 500 /log/weft/coordinator/main.log | awk '{print $2}' | sort -u | wc -l`,
			want:     true,
		},
		{
			name:     "recent-sample: tail | jq | sort | head -1",
			pipeline: `tail -n 5 /log/weft/coordinator/runtime.log | jq -r .fds_open | sort -rn | head -1`,
			want:     false,
		},
		{
			name:     "runtime-field bin script (single most-recent sample)",
			pipeline: `runtime-field coordinator fds_open`,
			want:     false,
		},
		{
			name:     "no cap at all (bare tail default -n 10, but no aggregate stage)",
			pipeline: `tail /log/weft/shard/metrics.log | jq -r 'select(.type=="disk")'`,
			want:     false,
		},
		{
			name:     "uncapped cat with a real filter, no head/tail",
			pipeline: `cat /log/weft/coordinator/main.log | grep -c ERROR`,
			want:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lp := lowerFor(t, c.pipeline)
			got := LintTruncationRisk(lp)
			if c.want && len(got) == 0 {
				t.Errorf("pipeline %q: want a truncation warning, got none (Limit=%d, ComputeStages=%v)",
					c.pipeline, lp.Limit, lp.ComputeStages)
			}
			if !c.want && len(got) != 0 {
				t.Errorf("pipeline %q: want no warning, got %v (Limit=%d, ComputeStages=%v)",
					c.pipeline, got, lp.Limit, lp.ComputeStages)
			}
		})
	}
}

// TestLintTruncationRisk_NilPlan guards the SaveCronWatch integration
// path, where a Lower() failure means loweredPlan is nil — the lint
// must no-op rather than panic.
func TestLintTruncationRisk_NilPlan(t *testing.T) {
	if got := LintTruncationRisk(nil); got != nil {
		t.Errorf("LintTruncationRisk(nil) = %v, want nil", got)
	}
}
