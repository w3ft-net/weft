package planner

import (
	"strings"
	"testing"
)

// TestPushdown_NewDimensions verifies the seven new dimension meta-vars
// (DEPLOYMENT, ENV, VERSION, REGION, CLUSTER, K8S_NAMESPACE, POD,
// CONTAINER) plus the SERVICE/HOST aliases push down to direct column
// predicates — the same shape today's $LEVEL / $APP / $INSTANCE produce.
// See docs/plans/ingest-dimensions.md.
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
		{"awk deployment", `cat vimgym.log | awk '$DEPLOYMENT == "shard"'`, "deployment = 'shard'"},
		{"awk env", `cat vimgym.log | awk '$ENV == "prod"'`, "env = 'prod'"},
		{"awk version", `cat vimgym.log | awk '$VERSION == "v1.2.3"'`, "version = 'v1.2.3'"},
		{"awk region", `cat vimgym.log | awk '$REGION == "us-east-2"'`, "region = 'us-east-2'"},
		{"awk cluster", `cat vimgym.log | awk '$CLUSTER == "prod-cluster"'`, "cluster = 'prod-cluster'"},
		{"awk k8s_namespace", `cat vimgym.log | awk '$K8S_NAMESPACE == "default"'`, "k8s_namespace = 'default'"},
		{"awk pod", `cat vimgym.log | awk '$POD == "api-7f9b"'`, "pod = 'api-7f9b'"},
		{"awk container", `cat vimgym.log | awk '$CONTAINER == "main"'`, "container = 'main'"},
		{"awk node", `cat vimgym.log | awk '$NODE == "shard-abc"'`, "node = 'shard-abc'"},

		// SERVICE alias for app — should rewrite to the same column.
		{"awk service alias", `cat vimgym.log | awk '$SERVICE == "api"'`, "app = 'api'"},
		// HOST alias for instance.
		{"awk host alias", `cat vimgym.log | awk '$HOST == "i-abc"'`, "instance = 'i-abc'"},

		// ── jq meta-vars ─────────────────────────────────────────────
		{"jq deployment", `cat vimgym.log | jq 'select(._deployment == "shard")'`, "deployment = 'shard'"},
		{"jq env", `cat vimgym.log | jq 'select(._env == "prod")'`, "env = 'prod'"},
		{"jq version", `cat vimgym.log | jq 'select(._version == "v1.2.3")'`, "version = 'v1.2.3'"},
		{"jq region", `cat vimgym.log | jq 'select(._region == "us-east-2")'`, "region = 'us-east-2'"},
		{"jq cluster", `cat vimgym.log | jq 'select(._cluster == "prod-cluster")'`, "cluster = 'prod-cluster'"},
		{"jq k8s_namespace", `cat vimgym.log | jq 'select(._k8s_namespace == "default")'`, "k8s_namespace = 'default'"},
		{"jq pod", `cat vimgym.log | jq 'select(._pod == "api-7f9b")'`, "pod = 'api-7f9b'"},
		{"jq container", `cat vimgym.log | jq 'select(._container == "main")'`, "container = 'main'"},
		{"jq node", `cat vimgym.log | jq 'select(._node == "shard-abc")'`, "node = 'shard-abc'"},

		// jq aliases.
		{"jq service alias", `cat vimgym.log | jq 'select(._service == "api")'`, "app = 'api'"},
		{"jq host alias", `cat vimgym.log | jq 'select(._host == "i-abc")'`, "instance = 'i-abc'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
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

// TestPathLayout_EnvSegment verifies a 3-level pseudofile path
// /log/<ns>/<env>/<app>.log produces both an app filter and an env
// filter — the env layer slots in between namespace and app-leaf.
// See docs/plans/ingest-dimensions.md.
func TestPathLayout_EnvSegment(t *testing.T) {
	tests := []struct {
		name      string
		pipeline  string
		wantApp   string // app predicate fragment
		wantEnv   string // env predicate fragment ("" = no env predicate expected)
	}{
		{
			name:     "two-level path (no env)",
			pipeline: `cat /log/weft/coordinator.log`,
			wantApp:  "app = 'weft/coordinator'",
			wantEnv:  "", // no env predicate in 2-level paths
		},
		{
			name:     "three-level telemetry path",
			pipeline: `cat /log/weft/coordinator/runtime.log`,
			wantApp:  "app = 'weft/coordinator/runtime'",
			wantEnv:  "", // no env predicate in 3-level paths
		},
		{
			name:     "three-level metrics path",
			pipeline: `tail /log/weft/coordinator/metrics.log`,
			wantApp:  "app = 'weft/coordinator/metrics'",
			wantEnv:  "",
		},
		{
			name:     "four-level path with env",
			pipeline: `cat /log/weft/prod/coordinator/runtime.log`,
			wantApp:  "app = 'weft/coordinator/runtime'",
			wantEnv:  "env = 'prod'",
		},
		{
			name:     "four-level path with staging",
			pipeline: `cat /log/myco/staging/api/metrics.log`,
			wantApp:  "app = 'myco/api/metrics'",
			wantEnv:  "env = 'staging'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Plan(tc.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tc.pipeline, err)
			}
			if !strings.Contains(plan.ShardSQL, tc.wantApp) {
				t.Errorf("ShardSQL missing app predicate %q\n  got: %s", tc.wantApp, plan.ShardSQL)
			}
			if tc.wantEnv == "" {
				if strings.Contains(plan.ShardSQL, "env = ") {
					t.Errorf("expected no env predicate, got: %s", plan.ShardSQL)
				}
			} else if !strings.Contains(plan.ShardSQL, tc.wantEnv) {
				t.Errorf("ShardSQL missing env predicate %q\n  got: %s", tc.wantEnv, plan.ShardSQL)
			}
		})
	}
}
