package planner

import (
	"testing"
	"time"
)

// TestNamespacePredicate verifies that the planner generates correct SQL
// predicates using the namespace/service/kind columns.
//
// Path layout:
//   1 seg → namespace wildcard (all services/kinds in namespace)
//   3 seg → namespace + service + kind (canonical)
//   4+ seg → namespace + service + kind (sub-stream rolled into kind)
//
// 2-seg /log/<ns>/<svc>.log is rejected — every log stream has an
// explicit kind, see TestKindPredicate_TwoSegRejected.
func TestNamespacePredicate(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "namespace-only query (1 segment)",
			pipeline: `cat /log/weft.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ?`,
		},
		{
			name:     "three-level: namespace + service + kind",
			pipeline: `cat /log/weft/coordinator/main.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
		},
		{
			name:     "bare name also namespace-only",
			pipeline: `cat vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ?`,
		},
		{
			name:     "head with namespace-only",
			pipeline: `head -5 /log/weft.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts ASC LIMIT 5`,
		},
		{
			name:     "tail with three-level path",
			pipeline: `tail -10 /log/weft/shard/main.log`,
			wantSQL:  `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? AND kind = ? ORDER BY ts DESC LIMIT 10) AS t ORDER BY ts ASC`,
		},
		{
			name:     "grep with three-level path",
			pipeline: `cat /log/weft/shard/main.log | grep ERROR`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? AND kind = ? AND regexp_matches(raw, 'ERROR')`,
		},
		{
			name:     "grep with namespace-only",
			pipeline: `cat /log/weft.log | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "awk filter with namespace-only",
			pipeline: `cat /log/weft.log | awk 'LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
		},
		{
			name:     "metrics stream (kind=metrics)",
			pipeline: `cat /log/weft/coordinator/metrics.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
		},
		{
			name:     "multi-arg cat unions kinds within a service",
			pipeline: `cat /log/weft/coordinator/main.log /log/weft/coordinator/heartbeats.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND service = ? AND kind IN (?, ?)`,
		},
		{
			name:     "multi-arg cat across services unions services and kinds",
			pipeline: `cat /log/weft/coordinator/main.log /log/weft/shard/main.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND service IN (?, ?) AND kind = ?`,
		},
		{
			name:     "four-level path: env-bearing ns/env/svc/kind",
			pipeline: `cat /log/weft/prod/coordinator/main.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE env = ? AND namespace = ? AND service = ? AND kind = ?`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
		})
	}
}
