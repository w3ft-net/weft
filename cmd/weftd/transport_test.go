package main

import (
	"context"
	"testing"
)

// The production default must be the TLS listener, never the plaintext one.
func TestDefaultEndpointIsTLSPort(t *testing.T) {
	if defaultEndpoint != "ingest.w3ft.net:9443" {
		t.Fatalf("defaultEndpoint = %q, want ingest.w3ft.net:9443", defaultEndpoint)
	}
}

// The bearer token must only ride a connection that is encrypted.
func TestBearerRequiresTransportSecurityFollowsTLS(t *testing.T) {
	if !(bearerToken{token: "t", secure: true}).RequireTransportSecurity() {
		t.Fatal("token over TLS must require transport security")
	}
	if (bearerToken{token: "t", secure: false}).RequireTransportSecurity() {
		t.Fatal("plaintext dev path must not require it")
	}
	md, err := bearerToken{token: "t", clientID: "weftd/x"}.GetRequestMetadata(context.Background())
	if err != nil || md["authorization"] != "Bearer t" {
		t.Fatalf("metadata = %v, %v", md, err)
	}
}
