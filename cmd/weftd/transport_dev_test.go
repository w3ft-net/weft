//go:build weftd_dev

package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDevOverridesParse(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("endpoint: coordinator:9020\ntls: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.endpoint() != "coordinator:9020" || cfg.tlsEnabled() {
		t.Fatalf("got endpoint=%q tls=%v", cfg.endpoint(), cfg.tlsEnabled())
	}
}

func TestDevDefaultsAreProduction(t *testing.T) {
	var cfg Config
	if cfg.endpoint() != defaultEndpoint || !cfg.tlsEnabled() {
		t.Fatalf("unset dev config: endpoint=%q tls=%v", cfg.endpoint(), cfg.tlsEnabled())
	}
}
