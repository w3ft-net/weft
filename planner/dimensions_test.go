package planner

import (
	"strings"
	"testing"
	"time"
)

// TestPushdown_NewDimensions verifies the dimension meta-vars
// (DEPLOYMENT, ENV, VERSION, REGION, CLUSTER, K8S_NAMESPACE, POD,
// CONTAINER) plus the SERVICE/HOST aliases push down to direct column
// predicates — the same shape LEVEL/INSTANCE produce.
func TestPushdown_NewDimensions(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		// We assert the WHERE fragment is included verbatim — equivalent
		// to the existing pushdown_test.go shape but tolerant of the
		// app-prefix noise from the source.
		wantWHERE string
	}{
		// ── awk meta-vars ────────────────────────────────────────────
		{"awk deployment", `cat vimgym.log | awk 'DEPLOYMENT == "shard"'`, "deployment = 'shard'"},
		{"awk env", `cat vimgym.log | awk 'ENV == "prod"'`, "env = 'prod'"},
		{"awk version", `cat vimgym.log | awk 'VERSION == "v1.2.3"'`, "version = 'v1.2.3'"},
		{"awk region", `cat vimgym.log | awk 'REGION == "us-east-2"'`, "region = 'us-east-2'"},
		{"awk cluster", `cat vimgym.log | awk 'CLUSTER == "prod-cluster"'`, "cluster = 'prod-cluster'"},
		{"awk k8s_namespace", `cat vimgym.log | awk 'K8S_NAMESPACE == "default"'`, "k8s_namespace = 'default'"},
		{"awk pod", `cat vimgym.log | awk 'POD == "api-7f9b"'`, "pod = 'api-7f9b'"},
		{"awk container", `cat vimgym.log | awk 'CONTAINER == "main"'`, "container = 'main'"},
		{"awk node", `cat vimgym.log | awk 'NODE == "shard-abc"'`, "node = 'shard-abc'"},
		{"awk trace_id", `cat vimgym.log | awk 'TRACE_ID == "trace-abc-123"'`, "trace_id = 'trace-abc-123'"},

		// SERVICE alias for app — should rewrite to the same column.
		{"awk service alias", `cat vimgym.log | awk 'SERVICE == "api"'`, "namespace = ?"},
		// HOST alias for instance.
		{"awk host alias", `cat vimgym.log | awk 'HOST == "i-abc"'`, "instance = 'i-abc'"},

		// ── jq meta-vars ─────────────────────────────────────────────
		{"jq deployment", `cat vimgym.log | jq 'select($deployment == "shard")'`, "deployment = 'shard'"},
		{"jq env", `cat vimgym.log | jq 'select($env == "prod")'`, "env = 'prod'"},
		{"jq version", `cat vimgym.log | jq 'select($version == "v1.2.3")'`, "version = 'v1.2.3'"},
		{"jq region", `cat vimgym.log | jq 'select($region == "us-east-2")'`, "region = 'us-east-2'"},
		{"jq cluster", `cat vimgym.log | jq 'select($cluster == "prod-cluster")'`, "cluster = 'prod-cluster'"},
		{"jq k8s_namespace", `cat vimgym.log | jq 'select($k8s_namespace == "default")'`, "k8s_namespace = 'default'"},
		{"jq pod", `cat vimgym.log | jq 'select($pod == "api-7f9b")'`, "pod = 'api-7f9b'"},
		{"jq container", `cat vimgym.log | jq 'select($container == "main")'`, "container = 'main'"},
		{"jq node", `cat vimgym.log | jq 'select($node == "shard-abc")'`, "node = 'shard-abc'"},
		{"jq trace_id", `cat vimgym.log | jq 'select($trace_id == "trace-abc-123")'`, "trace_id = 'trace-abc-123'"},

		// jq aliases.
		{"jq service alias", `cat vimgym.log | jq 'select($service == "api")'`, "namespace = ?"},
		{"jq host alias", `cat vimgym.log | jq 'select($host == "i-abc")'`, "instance = 'i-abc'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tc.pipeline, err)
			}
			if !strings.Contains(plan.ShardSQL, tc.wantWHERE) {
				t.Errorf("ShardSQL missing %q\n  got: %s", tc.wantWHERE, plan.ShardSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected full pushdown, got compute stages: %v", plan.ComputeStages)
			}
		})
	}
}

// TestPathLayout_KindSegment verifies pseudofile path parsing into
// namespace/env/service/kind predicates.
//
// Path layout:
//
//	3 seg → <ns>/<svc>/<kind>          env-less
//	4 seg → <ns>/<env>/<svc>/<kind>    env-bearing
//
// 2-seg paths are rejected (see TestKindPredicate_TwoSegRejected).
func TestPathLayout_KindSegment(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantNS   string
		wantSvc  string
		wantKind string
		wantEnv  bool
	}{
		{
			name:     "three-level path: main log (env-less)",
			pipeline: `cat /log/weft/coordinator/main.log`,
			wantNS:   "namespace = ?",
			wantSvc:  "service = ?",
			wantKind: "kind = ?",
			wantEnv:  false,
		},
		{
			name:     "three-level path: heartbeats (env-less)",
			pipeline: `cat /log/weft/compute/heartbeats.log`,
			wantNS:   "namespace = ?",
			wantSvc:  "service = ?",
			wantKind: "kind = ?",
			wantEnv:  false,
		},
		{
			name:     "four-level path: env-bearing ns/env/svc/kind",
			pipeline: `cat /log/loom/prod/api/heartbeats.log`,
			wantNS:   "namespace = ?",
			wantSvc:  "service = ?",
			wantKind: "kind = ?",
			wantEnv:  true,
		},
		{
			name:     "metrics under a service (env-less)",
			pipeline: `cat /log/myco/api/metrics.log`,
			wantNS:   "namespace = ?",
			wantSvc:  "service = ?",
			wantKind: "kind = ?",
			wantEnv:  false,
		},
		{
			name:     "tail env-bearing",
			pipeline: `tail /log/loom/sand/api/main.log`,
			wantNS:   "namespace = ?",
			wantSvc:  "service = ?",
			wantKind: "kind = ?",
			wantEnv:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tc.pipeline, err)
			}
			if !strings.Contains(plan.ShardSQL, tc.wantNS) {
				t.Errorf("ShardSQL missing namespace predicate %q\n  got: %s", tc.wantNS, plan.ShardSQL)
			}
			if tc.wantSvc != "" && !strings.Contains(plan.ShardSQL, tc.wantSvc) {
				t.Errorf("ShardSQL missing service predicate %q\n  got: %s", tc.wantSvc, plan.ShardSQL)
			}
			if tc.wantKind != "" && !strings.Contains(plan.ShardSQL, tc.wantKind) {
				t.Errorf("ShardSQL missing kind predicate %q\n  got: %s", tc.wantKind, plan.ShardSQL)
			}
			hasEnv := strings.Contains(plan.ShardSQL, "env = ")
			if hasEnv != tc.wantEnv {
				t.Errorf("env predicate present=%v, want %v\n  got: %s", hasEnv, tc.wantEnv, plan.ShardSQL)
			}
		})
	}
}
