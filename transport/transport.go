// Package transport is how the weft processes secure the links between them.
//
// Out of the box every link (api to coordinator, coordinator to compute and
// shards, shards to the coordinator) is plaintext gRPC and the api serves plain
// HTTP: the processes are meant to share a private network, with TLS terminated
// in front of the api. For a deployment that wants encryption on the links
// themselves, every process takes the same three settings:
//
//	--tls-cert, --tls-key   serve TLS with this certificate (api HTTP, every gRPC listener)
//	--tls-ca                dial peers over TLS, trusting the certificates this PEM bundle signs
//
// each also read from WEFT_TLS_CERT, WEFT_TLS_KEY and WEFT_TLS_CA. Set the CA on
// every process that dials another and the cert and key on every process that is
// dialed; a process can be both. The coordinator-to-shard lifecycle HTTP
// (/health, /transition, /shutdown), the health ports and the localhost-only
// admin and pprof ports stay plaintext: keep them on the private network.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Config is the process's TLS settings.
type Config struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// DefineFlags registers --tls-cert, --tls-key and --tls-ca on fs, defaulting to
// the WEFT_TLS_* environment.
func DefineFlags(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.CertFile, "tls-cert", os.Getenv("WEFT_TLS_CERT"), "PEM certificate to serve TLS with (with --tls-key); empty serves plaintext")
	fs.StringVar(&c.KeyFile, "tls-key", os.Getenv("WEFT_TLS_KEY"), "PEM private key for --tls-cert")
	fs.StringVar(&c.CAFile, "tls-ca", os.Getenv("WEFT_TLS_CA"), "PEM CA bundle: dial peers over TLS, trusting it; empty dials plaintext")
	return c
}

type loaded struct {
	server *tls.Config
	client credentials.TransportCredentials
}

var current atomic.Pointer[loaded]

// Apply checks the settings and makes them the process's: ServerOptions,
// ServerTLS and DialOption answer from them. A cert without a key (or the
// reverse), or a file that cannot be read, is an error.
func (c *Config) Apply() error {
	l := &loaded{}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("--tls-cert and --tls-key go together")
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		l.server = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return fmt.Errorf("read --tls-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("--tls-ca %s holds no certificate", c.CAFile)
		}
		l.client = credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}
	current.Store(l)
	return nil
}

// ServerTLS is the TLS configuration to serve with, nil for plaintext.
func ServerTLS() *tls.Config {
	if l := current.Load(); l != nil {
		return l.server
	}
	return nil
}

// ServerOptions are the options a gRPC server takes: its TLS credentials, or none.
func ServerOptions() []grpc.ServerOption {
	if t := ServerTLS(); t != nil {
		return []grpc.ServerOption{grpc.Creds(credentials.NewTLS(t))}
	}
	return nil
}

// DialOption is the transport credentials every gRPC dial uses: TLS against the
// configured CA, or plaintext.
func DialOption() grpc.DialOption {
	if l := current.Load(); l != nil && l.client != nil {
		return grpc.WithTransportCredentials(l.client)
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials())
}
