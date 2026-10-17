package main

import (
	"crypto/tls"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// legacyPlaintextEndpoint is the public NLB's plaintext listener, which
// every config written before the TLS listener existed names explicitly (the
// shard/loom startup scripts, hand-written host configs).
const legacyPlaintextEndpoint = "ingest.w3ft.net:9020"

// tlsEnabled resolves the tls: gate. Unset means on: the ingest stream
// carries the bearer token and the log lines, so it is encrypted unless an
// operator opts out (tls: false, for a dockerized or loopback coordinator
// that serves plaintext).
func (c Config) tlsEnabled() bool { return c.TLS == nil || *c.TLS }

// resolveEndpoint returns the address to dial. With TLS on, a config that
// still names the plaintext listener is pointed at the TLS one rather than
// left to fail every handshake: an agent that cannot connect never reaches
// the self-update path that would repair it, the same trap decideStartup
// guards against. moved reports that the address was changed, for logging.
// Drop this once the plaintext listener is gone.
func resolveEndpoint(cfg Config) (endpoint string, moved bool) {
	if cfg.tlsEnabled() && cfg.Endpoint == legacyPlaintextEndpoint {
		return defaultEndpoint, true
	}
	return cfg.Endpoint, false
}

// transportCreds is the channel security for the coordinator connection:
// verified TLS against the system roots, or plaintext when the config opts out.
func transportCreds(useTLS bool) credentials.TransportCredentials {
	if !useTLS {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}
