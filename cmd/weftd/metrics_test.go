package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func decodePayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload %q: %v", payload, err)
	}
	return m
}

// TestMetricRow: a row is its kind, the section fields, and the ts, host
// and deployment every row carries; the kind says what it is, so there's
// no "type" field.
func TestMetricRow(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	fields := map[string]any{
		"cpu":    map[string]any{"user": 12.5},
		"memory": map[string]any{"pct": 40.0},
	}
	row := metricRow(kindHost, "i-123", "prod-api", now, fields)

	if row.Kind != "host" || row.Instance != "i-123" || row.TsUnixNs != now.UnixNano() {
		t.Errorf("row = kind %q instance %q ts %d", row.Kind, row.Instance, row.TsUnixNs)
	}
	got := decodePayload(t, row.PayloadJson)
	if row.Raw != string(row.PayloadJson) {
		t.Errorf("raw %q != payload %q", row.Raw, row.PayloadJson)
	}
	if got["host"] != "i-123" || got["deployment"] != "prod-api" || got["ts"] != "2026-10-18T12:00:00Z" {
		t.Errorf("common fields = %v", got)
	}
	if _, ok := got["type"]; ok {
		t.Errorf("payload has a type field: %v", got)
	}
	if cpu, _ := got["cpu"].(map[string]any); cpu["user"] != 12.5 {
		t.Errorf("cpu = %v", got["cpu"])
	}
	if _, ok := fields["ts"]; ok {
		t.Errorf("metricRow wrote into the caller's fields: %v", fields)
	}
}

func TestMetricRow_DiskAndNetwork(t *testing.T) {
	now := time.Now()
	disk := metricRow(kindDisk, "i-1", "d", now, map[string]any{"path": "/var/lib", "pct": 91.0})
	if disk.Kind != "disk" {
		t.Errorf("disk kind = %q", disk.Kind)
	}
	if got := decodePayload(t, disk.PayloadJson); got["path"] != "/var/lib" || got["pct"] != 91.0 {
		t.Errorf("disk payload = %v", got)
	}
	net := metricRow(kindNetwork, "i-1", "d", now, map[string]any{"iface": "eth0"})
	if net.Kind != "network" {
		t.Errorf("network kind = %q", net.Kind)
	}
}

func TestCPUSection(t *testing.T) {
	prev := &cpuState{user: 100, system: 50, idle: 800, iowait: 50}
	cur := &cpuState{user: 150, system: 70, idle: 880, iowait: 100} // dt = 200
	got := cpuSection(prev, cur)
	for k, want := range map[string]float64{"user": 25, "system": 10, "idle": 40, "iowait": 25} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if cores, _ := got["cores"].(int); cores < 1 {
		t.Errorf("cores = %v", got["cores"])
	}
}

// TestNetworkRows: an interface is reported once both samples have it,
// as bytes per second.
func TestNetworkRows(t *testing.T) {
	prev := map[string]netState{"eth0": {rxBytes: 1000, txBytes: 500}}
	cur := map[string]netState{
		"eth0": {rxBytes: 3000, txBytes: 1500},
		"eth1": {rxBytes: 9, txBytes: 9}, // new since the last sample
	}
	rows := networkRows(prev, cur, 2)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want eth0 only", rows)
	}
	want := map[string]any{"iface": "eth0", "rx_bytes_sec": int64(1000), "tx_bytes_sec": int64(500)}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("%s = %v, want %v", k, rows[0][k], v)
		}
	}
}

func TestSanitizeDeployment(t *testing.T) {
	for in, want := range map[string]string{
		"prod-api":    "prod-api",
		" prod api ":  "prod_api",
		"a/b":         "a_b",
		"api.prod\tx": "api_prod_x",
		"":            "",
		"   ":         "",
	} {
		if got := sanitizeDeployment(in); got != want {
			t.Errorf("sanitizeDeployment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEffectiveDeployment: the deployment metrics are filed under is the
// one the rows carry, so a `deployment` metavariable with a value wins over
// the `deployment:` key, and an empty one falls back to it.
func TestEffectiveDeployment(t *testing.T) {
	withFreshMetavariables(t)
	prev := globalDeployment
	t.Cleanup(func() { globalDeployment = prev })
	globalDeployment = "from-config"
	ctx := context.Background()

	if got := effectiveDeployment(); got != "from-config" {
		t.Errorf("no metavariable: %q, want from-config", got)
	}

	cfg := map[string]MetavariableConfig{"deployment": {Env: "WEFTD_TEST_DEPLOYMENT"}}
	t.Setenv("WEFTD_TEST_DEPLOYMENT", "from-env")
	if err := initHost(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := effectiveDeployment(); got != "from-env" {
		t.Errorf("metavariable set: %q, want from-env", got)
	}

	t.Setenv("WEFTD_TEST_DEPLOYMENT", "")
	if err := initHost(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := effectiveDeployment(); got != "from-config" {
		t.Errorf("metavariable empty: %q, want from-config", got)
	}

	globalDeployment = ""
	if got := effectiveDeployment(); got != "" {
		t.Errorf("nothing set: %q, want empty", got)
	}
}
