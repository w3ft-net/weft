package main

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(b bool) *bool { return &b }

func TestTLSEnabled(t *testing.T) {
	cases := []struct {
		name string
		tls  *bool
		want bool
	}{
		{"unset defaults on", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false opts out", boolPtr(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Config{TLS: c.tls}).tlsEnabled(); got != c.want {
				t.Fatalf("tlsEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTLSConfigKeyParses(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("endpoint: coordinator:9020\ntls: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.tlsEnabled() {
		t.Fatal("tls: false in YAML did not disable TLS")
	}
}

// The production default must be the TLS listener, never the plaintext one.
func TestDefaultEndpointIsTLSPort(t *testing.T) {
	if defaultEndpoint != "ingest.w3ft.net:9443" {
		t.Fatalf("defaultEndpoint = %q, want ingest.w3ft.net:9443", defaultEndpoint)
	}
}

// Configs written when the plaintext listener was the only one name it
// explicitly. Under TLS that dial can only fail the handshake, so it moves to
// the TLS port; nothing else is rewritten.
func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		name      string
		cfg       Config
		wantEP    string
		wantMoved bool
	}{
		{"default untouched", Config{Endpoint: defaultEndpoint}, defaultEndpoint, false},
		{"legacy plaintext port moves under tls", Config{Endpoint: "ingest.w3ft.net:9020"}, "ingest.w3ft.net:9443", true},
		{"legacy port kept when tls is off", Config{Endpoint: "ingest.w3ft.net:9020", TLS: boolPtr(false)}, "ingest.w3ft.net:9020", false},
		{"dev endpoint untouched", Config{Endpoint: "coordinator:9020", TLS: boolPtr(false)}, "coordinator:9020", false},
		{"other host on 9020 untouched", Config{Endpoint: "10.0.0.5:9020"}, "10.0.0.5:9020", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ep, moved := resolveEndpoint(c.cfg)
			if ep != c.wantEP || moved != c.wantMoved {
				t.Fatalf("resolveEndpoint() = (%q, %v), want (%q, %v)", ep, moved, c.wantEP, c.wantMoved)
			}
		})
	}
}

// The bearer token must only ride a connection that is encrypted.
func TestBearerRequiresTransportSecurityFollowsTLS(t *testing.T) {
	if !(bearerToken{token: "t", secure: true}).RequireTransportSecurity() {
		t.Fatal("token over TLS must require transport security")
	}
	if (bearerToken{token: "t", secure: false}).RequireTransportSecurity() {
		t.Fatal("plaintext (tls: false) dev path must not require it")
	}
	md, err := bearerToken{token: "t", clientID: "weftd/x"}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer t" {
		t.Fatalf("metadata = %v, %v", md, err)
	}
}
