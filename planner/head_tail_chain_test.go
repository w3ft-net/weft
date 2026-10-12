package planner

import (
	"testing"
	"time"
)

// A head/tail after an already-limited source must act on the limited
// stream, not replace its LIMIT. `head -n 3 | tail -n 1` is the third
// row, not the newest row; `head -n 3 | head -n 10` is still three rows.
func TestPushdown_ChainedHeadTail(t *testing.T) {
	const src = `/log/acme/prod/api/main.log`
	const where = `WHERE env = ? AND namespace = ? AND service = ? AND kind = ?`
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:        "head then tail keeps the head window",
			pipeline:    `cat ` + src + ` | head -n 3 | tail -n 1`,
			wantSQL:     `SELECT raw, ts FROM logs ` + where + ` ORDER BY ts ASC LIMIT 3`,
			wantCompute: []string{"tail -n 1"},
		},
		{
			name:        "tail then head keeps the tail window",
			pipeline:    `cat ` + src + ` | tail -n 3 | head -n 1`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs ` + where + ` ORDER BY ts DESC LIMIT 3) AS t ORDER BY ts ASC`,
			wantCompute: []string{"head -n 1"},
		},
		{
			name:        "head then larger head stays at the smaller limit",
			pipeline:    `cat ` + src + ` | head -n 3 | head -n 10`,
			wantSQL:     `SELECT raw, ts FROM logs ` + where + ` ORDER BY ts ASC LIMIT 3`,
			wantCompute: []string{"head -n 10"},
		},
		{
			name:        "tail then larger tail stays at the smaller limit",
			pipeline:    `cat ` + src + ` | tail -n 3 | tail -n 10`,
			wantSQL:     `SELECT raw, ts FROM (SELECT raw, ts FROM logs ` + where + ` ORDER BY ts DESC LIMIT 3) AS t ORDER BY ts ASC`,
			wantCompute: []string{"tail -n 10"},
		},
		{
			name:        "head then smaller head is applied in compute",
			pipeline:    `cat ` + src + ` | head -n 10 | head -n 3`,
			wantSQL:     `SELECT raw, ts FROM logs ` + where + ` ORDER BY ts ASC LIMIT 10`,
			wantCompute: []string{"head -n 3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages: got %v, want %v", plan.ComputeStages, tt.wantCompute)
			}
			for i := range tt.wantCompute {
				if plan.ComputeStages[i] != tt.wantCompute[i] {
					t.Errorf("ComputeStages[%d]: got %q, want %q", i, plan.ComputeStages[i], tt.wantCompute[i])
				}
			}
		})
	}
}
