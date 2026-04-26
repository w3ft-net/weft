package planner

import "testing"

// TestNamespacePredicate verifies that the planner generates correct SQL
// predicates for namespace-level vs sub-app-level queries.
//
// Namespace queries (no "/" in app name) must use an OR predicate to match
// both the namespace itself and all sub-apps: (app = 'X' OR app LIKE 'X/%').
// Sub-app queries (containing "/") must use exact match: app = 'X/Y'.
func TestNamespacePredicate(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "namespace query uses OR with LIKE",
			pipeline: `cat /log/weft.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%')`,
		},
		{
			name:     "sub-app query uses exact match",
			pipeline: `cat /log/weft/coordinator.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app = 'weft/coordinator'`,
		},
		{
			name:     "flat app also uses namespace pattern",
			pipeline: `cat vimgym.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
		},
		{
			name:     "head with namespace",
			pipeline: `head -5 /log/weft.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') ORDER BY ts ASC LIMIT 5`,
		},
		{
			name:     "tail with sub-app",
			pipeline: `tail -10 /log/weft/shard.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app = 'weft/shard' ORDER BY ts DESC LIMIT 10`,
		},
		{
			name:     "grep with sub-app",
			pipeline: `cat /log/weft/shard.log | grep ERROR`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app = 'weft/shard' AND raw LIKE '%ERROR%'`,
		},
		{
			name:     "grep with namespace",
			pipeline: `cat /log/weft.log | grep timeout`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') AND raw LIKE '%timeout%'`,
		},
		{
			name:     "awk filter with namespace",
			pipeline: `cat /log/weft.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') AND level = 'ERROR'`,
		},
		{
			name:     "metrics sub-app uses exact match",
			pipeline: `cat /log/weft/metrics.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app = 'weft/metrics'`,
		},
		{
			// `cat *.log` (after glob expansion at the call site) lands here
			// as a multi-arg cat. The planner must union the apps with a
			// single IN predicate, not silently drop everything past args[0].
			name:     "multi-arg cat unions apps with IN predicate",
			pipeline: `cat /log/weft/coordinator.log /log/weft/shard.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app IN ('weft/coordinator', 'weft/shard')`,
		},
		{
			// All args share the same env segment → the env predicate is
			// preserved alongside the app IN list.
			name:     "multi-arg cat preserves shared env predicate",
			pipeline: `cat /log/acme/prod/api.log /log/acme/prod/worker.log`,
			wantSQL:  `SELECT line_num, raw FROM logs WHERE app IN ('acme/api', 'acme/worker') AND env = 'prod'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
		})
	}
}
