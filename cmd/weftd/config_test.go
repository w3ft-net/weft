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
token: wft_file

x-anchors:
  base: &base
    kind: main

sources:
  - service: api
    path: /var/log/api/*.log
    <<: *base
  - service: worker
    path: /var/log/worker/*.log
    <<: *base
    kind: queries
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal config with x-anchors holder: %v", err)
	}

	if cfg.Token != "wft_file" {
		t.Fatalf("scalars not parsed: token=%q", cfg.Token)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(cfg.Sources))
	}
	// The merge key must have spliced the anchored fields into both.
	if cfg.Sources[0].Service != "api" || cfg.Sources[0].Path != "/var/log/api/*.log" {
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
	cfg, err := parseConfig(data)
	if err != nil {
		t.Fatalf("shipped config.yaml.example does not parse: %v", err)
	}
	if cfg.Token == "" || len(cfg.Sources) == 0 {
		t.Fatalf("example missing essentials: token=%q sources=%d",
			cfg.Token, len(cfg.Sources))
	}
	if cfg.Metavariables["version"].Env != "APP_VERSION" {
		t.Fatalf("example metavariables block not parsed: %+v", cfg.Metavariables)
	}
	if cfg.Metrics == nil {
		t.Fatalf("example top-level metrics block not parsed: %+v", cfg.Metrics)
	}
}

// The metrics: block turns host metrics on by being present, including a bare
// `metrics:` (YAML null), and its absence leaves them off.
func TestParseConfig_MetricsPresence(t *testing.T) {
	cases := []struct {
		name, src string
		on        bool
		interval  string
	}{
		{"absent", "token: t\n", false, ""},
		{"bare", "token: t\nmetrics:\n", true, ""},
		{"empty map", "token: t\nmetrics: {}\n", true, ""},
		{"with interval", "token: t\nmetrics:\n  interval: 10s\n", true, "10s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if (cfg.Metrics != nil) != tc.on {
				t.Fatalf("Metrics = %+v, want on=%v", cfg.Metrics, tc.on)
			}
			if tc.on && cfg.Metrics.Interval != tc.interval {
				t.Fatalf("Interval = %q, want %q", cfg.Metrics.Interval, tc.interval)
			}
		})
	}
}
