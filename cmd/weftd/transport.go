package main

import (
	"crypto/tls"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/w3ft-net/weft/grpckeepalive"
)

// transportConfig is where weftd ships and how: the coordinator's ingest address
// (required: there is no default) and whether the connection is encrypted (on
// unless `tls: false`, which is for a plaintext coordinator on a private network).
type transportConfig struct {
	Endpoint string `yaml:"endpoint"` // coordinator ingest address, host:port
	TLS      *bool  `yaml:"tls"`      // nil = on
}

func (t transportConfig) endpoint() string { return t.Endpoint }
func (t transportConfig) tlsEnabled() bool { return t.TLS == nil || *t.TLS }

// validate is why a config without an endpoint does not start.
func (t transportConfig) validate() error {
	if t.Endpoint == "" {
		return errors.New("`endpoint:` is required: the coordinator's ingest address, host:port")
	}
	return nil
}

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
// verified TLS against the system roots, or plaintext when the config opts out.
func transportCreds(useTLS bool) credentials.TransportCredentials {
	if !useTLS {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}
