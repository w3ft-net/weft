// Package apiapp is the HTTP api process, split from cmd/api the way package app
// is split from cmd/coordinator: flags, the store, the coordinator connection,
// the syslog listener and the server are the same everywhere, and what differs
// (who the users are and the product's own configuration) comes from an
// Adapter. docs/plans/tenant-scope.md, Phase 4.
package apiapp

import (
	"context"
	"flag"

	"github.com/graybeardcorp/weft/alerting"
	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/single/singleapi"
)

// Adapter is what a deployment supplies. A nil field does nothing.
type Adapter struct {
	// Identity authenticates the api's users and registers its own sign-in and
	// account routes. It is called once the store is open, and an error stops
	// the process. Nil: the open identity, one HTTP Basic credential from
	// WEFT_BASIC_AUTH_USER and WEFT_BASIC_AUTH_PASSWORD, which refuses to start
	// when they are not set.
	Identity func(db *store.Store) (weftapi.Identity, error)

	// Tenancy is who acts, what they may do and what they may read, over the
	// store. Nil: the open one, which refuses everything under a shape that
	// has a tenant.
	Tenancy func(db *store.Store) tenancy.Adapter

	// AlertTargets are the alert destination kinds the deployment sends beyond
	// the core's (webhook, email). The api only checks /etc/notify.yaml
	// against them when it is saved; the coordinator sends. Nil: the core's.
	AlertTargets func(db *store.Store) []alerting.AlertTarget

	// Routes adds the deployment's own routes to the api's mux (hosted:
	// billing, the operator's admin, integrations).
	Routes func(*weftapi.Routes)

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

// identity is the adapter's Identity over db, or the open one.
func (a Adapter) identity(db *store.Store) (weftapi.Identity, error) {
	if a.Identity != nil {
		return a.Identity(db)
	}
	return singleapi.FromEnv()
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
