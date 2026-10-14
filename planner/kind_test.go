package planner

import (
	"testing"
	"time"
)

// TestKindPredicate pins the ns/svc/kind path layout. Every log
// pseudofile is /log/<ns>/<svc>/<kind>.log: <kind> is the stream
// (main, heartbeats, metrics, runtime, …) emitted by a service.
//
// Regression context: between 2026-05-11 and 2026-05-14 the planner
// treated 3-seg paths as <ns>/<env>/<svc>, so /log/weft/compute/
// heartbeats.log emitted env='compute' AND service='heartbeats' and
// matched zero rows. Ingest stored the row as ns='weft', service=
// 'compute/heartbeats' (first-slash split). This test locks the
// corrected layout.
func TestKindPredicate(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "heartbeats: ns/svc/kind",
			pipeline: `cat /log/weft/compute/heartbeats.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"weft", "compute", "heartbeats"},
		},
		{
			name:     "main: ns/svc/main is the regular logs",
			pipeline: `cat /log/weft/compute/main.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"weft", "compute", "main"},
		},
		{
			name:     "metrics: ns/svc/kind",
			pipeline: `cat /log/weft/compute/metrics.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"weft", "compute", "metrics"},
		},
		{
			name:     "runtime: ns/svc/kind",
			pipeline: `cat /log/weft/coordinator/runtime.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"weft", "coordinator", "runtime"},
		},
		{
			name:     "multi-arg cat unions kinds within a service",
			pipeline: `cat /log/weft/compute/heartbeats.log /log/weft/compute/metrics.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind IN (?, ?)`,
			wantArgs: []any{"weft", "compute", "heartbeats", "metrics"},
		},
		{
			name:     "tail -f heartbeats",
			pipeline: `tail -10 /log/weft/compute/heartbeats.log`,
			wantSQL:  `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND service = ? AND kind = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 10) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantArgs: []any{"weft", "compute", "heartbeats"},
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
			if len(plan.Args) != len(tt.wantArgs) {
				t.Fatalf("Args length: got %d %v, want %d %v",
					len(plan.Args), plan.Args, len(tt.wantArgs), tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if plan.Args[i] != want {
					t.Errorf("Args[%d]: got %v, want %v", i, plan.Args[i], want)
				}
			}
		})
	}
}

// TestKindPredicate_TwoSegRejected asserts the hard cut: legacy
// 2-seg paths like /log/weft/compute.log no longer resolve. Users
// see a parse-time error pointing at the canonical 3-seg form rather
// than getting silent empty results.
func TestKindPredicate_TwoSegRejected(t *testing.T) {
	_, err := Plan(`cat /log/weft/compute.log`, 0, "", time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected error for 2-seg path /log/weft/compute.log, got nil")
	}
}

// TestEnvBearing_FourSeg pins the env-bearing shape: a 4-seg path
// /log/<ns>/<env>/<svc>/<kind>.log resolves to env+namespace+service+
// kind predicates. This is the form SeenServices.encodeName produces for
// env-bearing producer tokens (env spliced at position 1). Segment
// count alone disambiguates env-less (3) from env-bearing (4) — no
// caller context needed, which matters because reader JWTs span envs.
func TestEnvBearing_FourSeg(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "env-bearing main log",
			pipeline: `cat /log/loom/prod/api/main.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE env = ? AND namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"prod", "loom", "api", "main"},
		},
		{
			name:     "env-bearing heartbeats",
			pipeline: `cat /log/loom/sand/worker/heartbeats.log`,
			wantSQL:  `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE env = ? AND namespace = ? AND service = ? AND kind = ?`,
			wantArgs: []any{"sand", "loom", "worker", "heartbeats"},
		},
		{
			name:     "env-bearing tail",
			pipeline: `tail -10 /log/loom/prod/api/metrics.log`,
			wantSQL:  `SELECT raw, ts, event_id, line_no FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE env = ? AND namespace = ? AND service = ? AND kind = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 10) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantArgs: []any{"prod", "loom", "api", "metrics"},
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
			if len(plan.Args) != len(tt.wantArgs) {
				t.Fatalf("Args: got %d %v, want %d %v",
					len(plan.Args), plan.Args, len(tt.wantArgs), tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if plan.Args[i] != want {
					t.Errorf("Args[%d]: got %v, want %v", i, plan.Args[i], want)
				}
			}
		})
	}
}
