package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigError(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		expect string
	}{
		{"usable config", Config{Token: "wft_x", transportConfig: transportConfig{Endpoint: "ingest.example.com:9443"}}, ""},
		{"missing token", Config{transportConfig: transportConfig{Endpoint: "x:1"}}, "token is required: set token: in the config file or WEFTD_TOKEN in the environment"},
		{"missing endpoint", Config{Token: "wft_x"}, "`endpoint:` is required: the coordinator's ingest address, host:port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := configError(c.cfg); got != c.expect {
				t.Fatalf("configError() = %q, want %q", got, c.expect)
			}
		})
	}
}

// TLS is on unless the config says `tls: false`, in release builds too.
func TestTransportFromYAML(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("endpoint: coordinator:9020\ntls: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.endpoint() != "coordinator:9020" || cfg.tlsEnabled() {
		t.Fatalf("endpoint=%q tls=%v", cfg.endpoint(), cfg.tlsEnabled())
	}
	var def Config
	if !def.tlsEnabled() {
		t.Fatal("tls is off by default")
	}
}

func TestUpdateChecker(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	key := base64.StdEncoding.EncodeToString(pub)
	on := true

	if u, err := updateChecker(Config{}, "1.0.0"); u != nil || err != nil {
		t.Fatalf("no config: got %v, %v; want no updater", u, err)
	}
	if _, err := updateChecker(Config{AutoUpdate: &on}, "1.0.0"); err == nil {
		t.Fatal("auto_update without a manifest was accepted")
	}
	if _, err := updateChecker(Config{UpdateManifest: "https://r.example/m.json"}, "1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "update_public_key") {
		t.Fatalf("a manifest without a key was accepted: %v", err)
	}
	u, err := updateChecker(Config{AutoUpdate: &on, UpdateManifest: "https://r.example/m.json", UpdatePublicKey: key}, "1.0.0")
	if err != nil || u == nil || !u.AutoApply {
		t.Fatalf("full config: %v, %v", u, err)
	}
	u, _ = updateChecker(Config{UpdateManifest: "https://r.example/m.json", UpdatePublicKey: key}, "1.0.0")
	if u == nil || u.AutoApply {
		t.Fatal("a manifest alone must only nag, not apply")
	}
}

func TestBakedInDefaultEndpoint(t *testing.T) {
	old := defaultEndpoint
	defaultEndpoint = "ingest.example.com:9443"
	defer func() { defaultEndpoint = old }()

	cfg := Config{Token: "wft_x"}
	if got := configError(cfg); got != "" {
		t.Fatalf("a config with no endpoint failed despite a baked-in default: %q", got)
	}
	if cfg.endpoint() != "ingest.example.com:9443" {
		t.Fatalf("endpoint() = %q", cfg.endpoint())
	}
	cfg.Endpoint = "other:1"
	if cfg.endpoint() != "other:1" {
		t.Fatalf("a configured endpoint lost to the default: %q", cfg.endpoint())
	}
}
