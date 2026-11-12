// Package apiapp is the HTTP api process, split from cmd/api the way package app
// is split from cmd/coordinator: flags, the store, the coordinator connection,
// the syslog listener and the server are the same everywhere, and what differs
// (the identity secret and the product's own configuration) comes from an
// Adapter. It imports package api, which is still the hosted product's, so
// unlike package app it does not build under the default shape yet; the next
// step splits the api's routes. docs/plans/tenant-scope.md, Phase 4.
package apiapp

import (
	"context"
	"errors"
	"flag"

	weftapi "github.com/graybeardcorp/weft/api"
)

// Adapter is what a deployment supplies. A nil field does nothing.
type Adapter struct {
	// JWTSecret is the secret the api signs and checks tokens with. Required.
	// It is called once, where the process reads its secrets, so a missing one
	// is reported at the same point on every start.
	JWTSecret func() string

	// Configure sets the deployment's own configuration on the server, once it
	// is connected to the coordinator (hosted: Stripe prices and keys, Slack
	// client).
	Configure func(srv *weftapi.Server)

	// Start begins the deployment's background work, before the syslog
	// listener (hosted: the sweep of accounts that never verified an email).
	Start func(ctx context.Context, srv *weftapi.Server)

	// Serving begins work that belongs beside the running listeners (hosted:
	// the daily Stripe usage reporter).
	Serving func(ctx context.Context, srv *weftapi.Server)
}

func (a Adapter) validate() error {
	if a.JWTSecret == nil {
		return errors.New("the adapter has no JWTSecret: an api that cannot check tokens does not start")
	}
	return nil
}

// Flags are the api's command-line flags. Their names, defaults and usage text
// are the process's interface to its unit files; testdata/api_flags.golden pins
// them.
type Flags struct {
	Port             string
	DBDSN            string
	CoordinatorAddr  string
	BaseURL          string
	RenderSyslogPort int
	RedisAddr        string
	RedisTLS         bool
}

// DefineFlags registers the api's flags on fs.
func DefineFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Port, "port", "8080", "HTTP listen port")
	fs.StringVar(&f.DBDSN, "db-dsn", "", "PostgreSQL DSN")
	fs.StringVar(&f.CoordinatorAddr, "coordinator", "localhost:9025", "coordinator gRPC address (its --local-addr; the public port serves only ingest RPCs)")
	fs.StringVar(&f.BaseURL, "base-url", "https://w3ft.net", "base URL for magic links")
	fs.IntVar(&f.RenderSyslogPort, "render-syslog-port", 9023, "TCP port for the Render TLS syslog listener (see docs/ingestion-syslog.md); the NLB terminates TLS and forwards plaintext here")
	fs.StringVar(&f.RedisAddr, "redis-addr", "", "Redis/Valkey address for the shared ingest-schema cache (host:port); empty disables it, every schema lookup falls through to Postgres directly")
	fs.BoolVar(&f.RedisTLS, "redis-tls", false, "use TLS for the Redis connection (required for ElastiCache in-transit encryption)")
	return f
}
