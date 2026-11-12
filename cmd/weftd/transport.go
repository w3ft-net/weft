package main

import (
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/graybeardcorp/weft/internal/grpckeepalive"
)

// defaultEndpoint is the production coordinator ingest address: the public
// NLB's TLS listener. Release builds ship only here, over TLS; the address
// and the TLS gate are not configurable. A dev build (-tags weftd_dev, see
// transport_dev.go) can override both to reach a plaintext local coordinator.
const defaultEndpoint = "ingest.w3ft.net:9443"

// coordKeepalive is how weftd tells a dead coordinator connection from a quiet
// one. A host behind a middlebox reaches the coordinator through it, and when
// that route is replaced its established connections die silently, so without
// pings weftd stops shipping until the kernel gives up.
var coordKeepalive = grpckeepalive.Client

// coordDialOptions are how weftd dials the coordinator: the channel security,
// the bearer token every RPC carries, and the keepalive that notices a
// connection that has gone silent.
func coordDialOptions(useTLS bool, creds credentials.PerRPCCredentials) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(transportCreds(useTLS)),
		grpc.WithPerRPCCredentials(creds),
		grpc.WithKeepaliveParams(coordKeepalive),
	}
}

// transportCreds is the channel security for the coordinator connection:
// verified TLS against the system roots, or plaintext when a dev build opts out.
func transportCreds(useTLS bool) credentials.TransportCredentials {
	if !useTLS {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}
