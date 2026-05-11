package planner

import (
	"testing"
	"time"
)

// TestNamespacePredicate verifies that the planner generates correct SQL
// predicates using the normalized namespace/service/env columns.
//
// Path layout:
//   1 seg → namespace wildcard (all services in namespace)
//   2 seg → namespace + service (no env)
//   3+ seg → namespace + env + service (env at position 1)
func TestNamespacePredicate(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "namespace-only query (1 segment)",
			pipeline: `cat /log/weft.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft'`,
		},
		{
			name:     "two-level: namespace + service, no env",
			pipeline: `cat /log/weft/coordinator.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND service = 'coordinator'`,
		},
		{
			name:     "bare name also namespace-only",
			pipeline: `cat vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'vimgym'`,
		},
		{
			name:     "head with namespace-only",
			pipeline: `head -5 /log/weft.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' ORDER BY ts ASC LIMIT 5`,
		},
		{
			name:     "tail with namespace+service (no env)",
			pipeline: `tail -10 /log/weft/shard.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND service = 'shard' ORDER BY ts DESC LIMIT 10`,
		},
		{
			name:     "grep with namespace+service",
			pipeline: `cat /log/weft/shard.log | grep ERROR`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND service = 'shard' AND regexp_matches(raw, 'ERROR')`,
		},
		{
			name:     "grep with namespace-only",
			pipeline: `cat /log/weft.log | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "awk filter with namespace-only",
			pipeline: `cat /log/weft.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND level = 'ERROR'`,
		},
		{
			name:     "two-level metrics (no env)",
			pipeline: `cat /log/weft/metrics.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND service = 'metrics'`,
		},
		{
			name:     "multi-arg cat unions services",
			pipeline: `cat /log/weft/coordinator.log /log/weft/shard.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'weft' AND service IN ('coordinator', 'shard')`,
		},
		{
			name:     "multi-arg cat with env preserves env predicate",
			pipeline: `cat /log/acme/prod/api/metrics.log /log/acme/prod/worker/metrics.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE env = 'prod' AND namespace = 'acme' AND service IN ('api/metrics', 'worker/metrics')`,
		},
		{
			name:     "three-level path: namespace + env + service",
			pipeline: `cat /log/loom/prod/api.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE env = 'prod' AND namespace = 'loom' AND service = 'api'`,
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
