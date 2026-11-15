package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"gopkg.in/yaml.v3"
)

// withFreshMetavariables swaps the package-level reader map for the test.
// It is global because it is populated once at startup and read on the
// row-stamping hot path.
func withFreshMetavariables(t *testing.T) {
	t.Helper()
	prev := metavariables
	metavariables = metavarSet{}
	t.Cleanup(func() {
		metavariables = prev
		sourceMetavariables.Range(func(k, _ any) bool { sourceMetavariables.Delete(k); return true })
	})
}

// initHost builds just the host-wide set, as a config with no sources would.
func initHost(ctx context.Context, cfg map[string]MetavariableConfig) error {
	_, err := initMetavariables(ctx, Config{Metavariables: cfg})
	return err
}

func tempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Each of env:, file: and json: yields the value its source holds, and the
// stamped row carries it.
func TestMetavariables_ThreeSources(t *testing.T) {
	withFreshMetavariables(t)
	t.Setenv("TEST_APP_VERSION", "v9-from-env")
	versionFile := tempFile(t, "VERSION", "  2026.10.18-3\n")
	identity := tempFile(t, "shard.json", `{"shard_id":"shard-xyz"}`)

	err := initHost(context.Background(), map[string]MetavariableConfig{
		"deployment": {Env: "TEST_APP_VERSION"},
		"version":    {File: versionFile},
		"node":       {JSON: identity + ":.shard_id"},
	})
	if err != nil {
		t.Fatal(err)
	}

	row := &pb.LogRow{Region: "us-east-2"}
	stampMetavariables(row, "")
	if row.Deployment != "v9-from-env" {
		t.Errorf("Deployment = %q, want the env value", row.Deployment)
	}
	if row.Version != "2026.10.18-3" {
		t.Errorf("Version = %q, want the trimmed file contents", row.Version)
	}
	if row.Node != "shard-xyz" {
		t.Errorf("Node = %q, want the JSON field", row.Node)
	}
	if row.Region != "us-east-2" {
		t.Errorf("Region = %q; an unconfigured metavariable must leave the row's own value", row.Region)
	}
}

// A configured metavariable overrides the value the row already carries, but
// one that resolves to "" does not blank it.
func TestMetavariables_OverrideAndEmpty(t *testing.T) {
	withFreshMetavariables(t)
	t.Setenv("TEST_REGION", "eu-west-1")
	t.Setenv("TEST_EMPTY", "")
	err := initHost(context.Background(), map[string]MetavariableConfig{
		"region":  {Env: "TEST_REGION"},
		"cluster": {Env: "TEST_EMPTY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &pb.LogRow{Region: "us-east-2", Cluster: "c1"}
	stampMetavariables(row, "")
	if row.Region != "eu-west-1" {
		t.Errorf("Region = %q, want the metavariable to override", row.Region)
	}
	if row.Cluster != "c1" {
		t.Errorf("Cluster = %q; an empty metavariable must not blank the row", row.Cluster)
	}
}

// A file: metavariable is live: rewriting the file changes later rows.
func TestMetavariables_FileIsLive(t *testing.T) {
	withFreshMetavariables(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := tempFile(t, "VERSION", "v1\n")
	if err := initHost(ctx, map[string]MetavariableConfig{"version": {File: path}}); err != nil {
		t.Fatal(err)
	}
	if got := metavarValue("", "version"); got != "v1" {
		t.Fatalf("version = %q, want v1", got)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "version to reload", func() bool { return metavarValue("", "version") == "v2" })
}

func TestMetavariables_BadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]MetavariableConfig
		want string
	}{
		{"none set", map[string]MetavariableConfig{"version": {}}, "exactly one"},
		{"two set", map[string]MetavariableConfig{"version": {Env: "X", File: "/f"}}, "exactly one"},
		{"unknown column", map[string]MetavariableConfig{"instance": {Env: "X"}}, "cannot be set here"},
		{"json without a path", map[string]MetavariableConfig{"node": {JSON: "/etc/shard.json"}}, "metavariables.node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFreshMetavariables(t)
			err := initHost(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestMetavariablesConfig_YAML(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
metavariables:
  version:
    env: APP_VERSION
  node:
    json: /opt/app/data/shard.json:.shard_id
  cluster:
    file: /etc/cluster
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metavariables["version"].Env != "APP_VERSION" ||
		cfg.Metavariables["node"].JSON != "/opt/app/data/shard.json:.shard_id" ||
		cfg.Metavariables["cluster"].File != "/etc/cluster" {
		t.Fatalf("metavariables not parsed: %+v", cfg.Metavariables)
	}
}

// An agent that auto-updates onto this build with an old config must be told
// that its dimensions: and per-source version: are no longer read.
func TestRetiredConfigKeys(t *testing.T) {
	old := []byte(`
dimensions:
  node: /opt/app/data/shard.json:.shard_id
sources:
  - path: /var/log/a.log
    version:
      env: APP_VERSION
`)
	if got := retiredConfigKeys(old); len(got) != 2 {
		t.Fatalf("retiredConfigKeys = %v, want both retired keys reported", got)
	}
	if got := retiredConfigKeys([]byte("metrics:\n  enabled: false\n")); len(got) != 1 {
		t.Fatalf("retiredConfigKeys = %v, want metrics.enabled reported (it now means ON)", got)
	}
	if got := retiredConfigKeys([]byte("checkpoint:\n  enabled: false\n")); len(got) != 1 {
		t.Fatalf("retiredConfigKeys = %v, want checkpoint.enabled reported (checkpointing is now always on)", got)
	}
	if got := retiredConfigKeys([]byte("metrics:\n  interval: 10s\ncheckpoint:\n  dir: /x\n")); len(got) != 0 {
		t.Fatalf("retiredConfigKeys = %v on a current metrics block, want none", got)
	}
	current := []byte("metavariables:\n  version:\n    env: V\nsources:\n  - path: /var/log/a.log\n")
	if got := retiredConfigKeys(current); len(got) != 0 {
		t.Fatalf("retiredConfigKeys = %v on a current config, want none", got)
	}
}

// A source's own metavariable wins for that source's rows; every other source
// and the host-level rows keep the host-wide value.
func TestMetavariables_SourceOverridesHost(t *testing.T) {
	withFreshMetavariables(t)
	t.Setenv("HOST_VERSION", "host-v1")
	t.Setenv("API_VERSION", "api-v2")
	t.Setenv("API_EMPTY", "")
	cfg := Config{
		Metavariables: map[string]MetavariableConfig{
			"version": {Env: "HOST_VERSION"},
			"cluster": {Env: "HOST_VERSION"},
		},
		Sources: []SourceConfig{
			{Service: "web"},
			{Service: "api", Metavariables: map[string]MetavariableConfig{
				"version": {Env: "API_VERSION"},
				"cluster": {Env: "API_EMPTY"},
			}},
		},
	}
	perSource, err := initMetavariables(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(perSource[0]) != 0 || len(perSource[1]) != 2 {
		t.Fatalf("per-source sets = %v, want none for web and two for api", perSource)
	}
	registerSource("ns/api/main", perSource[1])
	registerSource("ns/web/main", perSource[0])

	for _, tc := range []struct {
		app, wantVersion, wantCluster string
	}{
		{"ns/api/main", "api-v2", "host-v1"}, // its own version; an empty override falls back to the host
		{"ns/web/main", "host-v1", "host-v1"},
		{"ns/other/main", "host-v1", "host-v1"},
		{"", "host-v1", "host-v1"}, // host-level rows (metrics) have no source
	} {
		row := &pb.LogRow{}
		stampMetavariables(row, tc.app)
		if row.Version != tc.wantVersion || row.Cluster != tc.wantCluster {
			t.Errorf("app %q: version=%q cluster=%q, want %q and %q", tc.app, row.Version, row.Cluster, tc.wantVersion, tc.wantCluster)
		}
	}
}

// All files matched by one glob source share its metavariables.
func TestMetavariables_GlobFilesShareTheSourceSet(t *testing.T) {
	withFreshMetavariables(t)
	t.Setenv("GLOB_VERSION", "g1")
	set, err := buildMetavariables(context.Background(), "sources[0].metavariables",
		map[string]MetavariableConfig{"version": {Env: "GLOB_VERSION"}})
	if err != nil {
		t.Fatal(err)
	}
	registerSource("ns/api/main", set)
	registerSource("ns/worker/main", set)
	for _, app := range []string{"ns/api/main", "ns/worker/main"} {
		if got := metavarValue(app, "version"); got != "g1" {
			t.Errorf("%s version = %q, want g1", app, got)
		}
	}
}

func TestMetavariables_SourceBadConfigNamesTheSource(t *testing.T) {
	withFreshMetavariables(t)
	_, err := initMetavariables(context.Background(), Config{Sources: []SourceConfig{
		{Service: "a"},
		{Service: "b", Metavariables: map[string]MetavariableConfig{"version": {}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "sources[1].metavariables.version") {
		t.Fatalf("err = %v, want one naming sources[1].metavariables.version", err)
	}
}
