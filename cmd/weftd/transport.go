package main

import (
	"crypto/tls"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// defaultEndpoint is the production coordinator ingest address: the public
// NLB's TLS listener. Release builds ship only here, over TLS; the address
// and the TLS gate are not configurable. A dev build (-tags weftd_dev, see
// transport_dev.go) can override both to reach a plaintext local coordinator.
const defaultEndpoint = "ingest.w3ft.net:9443"

// transportCreds is the channel security for the coordinator connection:
// verified TLS against the system roots, or plaintext when a dev build opts out.
func transportCreds(useTLS bool) credentials.TransportCredentials {
	if !useTLS {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}
