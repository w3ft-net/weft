//go:build !weftd_dev

package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// Release builds ignore endpoint: and tls:. Configs written before the keys
// were removed (shard/loom startup scripts, hand-written host configs) still
// parse, but cannot redirect the agent or downgrade it to plaintext.
func TestReleaseIgnoresEndpointAndTLS(t *testing.T) {
	var cfg Config
	src := "endpoint: evil.example:9020\ntls: false\ntoken: wft_x\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.endpoint() != defaultEndpoint {
		t.Fatalf("endpoint() = %q, want %q", cfg.endpoint(), defaultEndpoint)
	}
	if !cfg.tlsEnabled() {
		t.Fatal("tls: false in YAML disabled TLS in a release build")
	}
	if cfg.Token != "wft_x" {
		t.Fatalf("token = %q", cfg.Token)
	}
}
