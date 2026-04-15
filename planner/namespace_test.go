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
			wantSQL:  `SELECT * FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%')`,
		},
		{
			name:     "sub-app query uses exact match",
			pipeline: `cat /log/weft/coordinator.log`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'weft/coordinator'`,
		},
		{
			name:     "flat app also uses namespace pattern",
			pipeline: `cat vimgym.log`,
			wantSQL:  `SELECT * FROM logs WHERE (app = 'vimgym' OR app LIKE 'vimgym/%')`,
		},
		{
			name:     "head with namespace",
			pipeline: `head -5 /log/weft.log`,
			wantSQL:  `SELECT * FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') ORDER BY ts ASC LIMIT 5`,
		},
		{
			name:     "tail with sub-app",
			pipeline: `tail -10 /log/weft/shard.log`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'weft/shard' ORDER BY ts DESC LIMIT 10`,
		},
		{
			name:     "grep with sub-app",
			pipeline: `cat /log/weft/shard.log | grep ERROR`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'weft/shard' AND raw LIKE '%ERROR%'`,
		},
		{
			name:     "grep with namespace",
			pipeline: `cat /log/weft.log | grep timeout`,
			wantSQL:  `SELECT * FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') AND raw LIKE '%timeout%'`,
		},
		{
			name:     "awk filter with namespace",
			pipeline: `cat /log/weft.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:  `SELECT * FROM logs WHERE (app = 'weft' OR app LIKE 'weft/%') AND level = 'ERROR'`,
		},
		{
			name:     "metrics sub-app uses exact match",
			pipeline: `cat /log/weft/metrics.log`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'weft/metrics'`,
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
