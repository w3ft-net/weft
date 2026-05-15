package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// The token credential chain is AWS-shaped: env wins over the authored
// file, file is the fallback, empty-everywhere stays empty (the caller
// turns that into a fatal).
func TestTokenFromChain(t *testing.T) {
	cases := []struct {
		name              string
		env, file, expect string
	}{
		{"env wins over file", "wft_env", "wft_file", "wft_env"},
		{"file fallback when env unset", "", "wft_file", "wft_file"},
		{"env wins even when file empty", "wft_env", "", "wft_env"},
		{"empty everywhere", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tokenFromChain(c.env, c.file); got != c.expect {
				t.Fatalf("tokenFromChain(%q, %q) = %q, want %q", c.env, c.file, got, c.expect)
			}
		})
	}
}

// The authored config may use a YAML anchor holder block to DRY
// repeated source fields. weftd uses plain yaml.Unmarshal (no
// KnownFields), so an unknown top-level key like x-anchors is ignored
// while its &anchors are still resolvable via *aliases / << merges.
// This pins that assumption — the whole "author YAML with anchors"
// design rests on it.
func TestConfigParsesAnchorHolderBlock(t *testing.T) {
	const src = `
endpoint: w3ft.net:443
token: wft_file

x-anchors:
  base: &base
    kind: main

sources:
  - app: api
    path: /var/log/api/*.log
    <<: *base
  - app: worker
    path: /var/log/worker/*.log
    <<: *base
    kind: queries
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal config with x-anchors holder: %v", err)
	}

	if cfg.Endpoint != "w3ft.net:443" || cfg.Token != "wft_file" {
		t.Fatalf("scalars not parsed: endpoint=%q token=%q", cfg.Endpoint, cfg.Token)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(cfg.Sources))
	}
	// The merge key must have spliced the anchored fields into both.
	if cfg.Sources[0].App != "api" || cfg.Sources[0].Path != "/var/log/api/*.log" {
		t.Fatalf("source 0 wrong: %+v", cfg.Sources[0])
	}
	// Merge spliced the anchored kind into source 0 (a real struct field).
	if cfg.Sources[0].Kind != "main" {
		t.Fatalf("source 0 kind = %q, want main (merged from anchor)", cfg.Sources[0].Kind)
	}
	// Own key wins over the merged one.
	if cfg.Sources[1].Kind != "queries" {
		t.Fatalf("source 1 kind = %q, want queries (own key overrides merge)", cfg.Sources[1].Kind)
	}
}

// The config.yaml.example shipped in the deb is the operator's starting
// point — it must parse against the real Config (anchors/merge keys and
// all) or every fresh install starts from a broken sample.
func TestShippedExampleParses(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	example := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "weftd", "config.yaml.example")

	data, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("read shipped example: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("shipped config.yaml.example does not parse: %v", err)
	}
	if cfg.Endpoint == "" || cfg.Token == "" || len(cfg.Sources) == 0 {
		t.Fatalf("example missing essentials: endpoint=%q token=%q sources=%d",
			cfg.Endpoint, cfg.Token, len(cfg.Sources))
	}
	// Source 0 merges *with_metrics from the x-anchors block. metrics is
	// a real SourceConfig field, so a silently-failed merge key would
	// leave it disabled — this proves the anchor splice actually lands.
	if !cfg.Sources[0].Metrics.Enabled {
		t.Fatalf("example source 0 metrics not enabled — <<: *with_metrics merge did not splice")
	}
}
