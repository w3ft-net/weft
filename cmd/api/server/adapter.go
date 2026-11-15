// Package server is the HTTP api process, split from cmd/api the way
// cmd/coordinator/server is split from cmd/coordinator: flags, the store, the
// coordinator connection, the syslog listener and the server are the same
// everywhere, and what differs (who the users are and the product's own
// configuration) comes from an Adapter. docs/plans/tenant-scope.md, Phase 4.
package server

import (
	"context"
	"fmt"

	"github.com/w3ft-net/weft/alerting"
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
	"github.com/w3ft-net/weft/tenancy"
)

// Adapter is what a deployment supplies. A nil field does nothing, except
// Open, Identity and Tenancy, which are required.
type Adapter[DB any] struct {
	// Identity authenticates the api's users and registers its own sign-in and
	// account routes. It is called once the store is open, and an error stops
	// the process. Required.
	Identity func(db DB) (weftapi.Identity, error)

	// Open opens the deployment's database from the DSN (--db-dsn, else DB_DSN,
	// else empty) and the Redis settings of the shared ingest-schema cache
	// (empty address: none), and returns it with what closes it. Hosted migrates
	// and connects Postgres, picks its own default for an empty DSN, and turns on
	// its auth cache. Required.
	Open func(dsn, redisAddr string, redisTLS bool) (db DB, closeDB func(), err error)

	// Tenancy is who acts, what they may do and what they may read, over the
	// store. Required.
	Tenancy func(db DB) tenancy.Adapter

	// Tenants says whether a partition names a tenant; the routes that read or
	// write a tenant's data refuse a session that acts in none. Nil: every
	// partition does.
	Tenants func(shape.Added) bool

	// Tokens is where agent tokens live, Sources where namespaces and sources
	// do, Crons where crons and their results do, Bins where /bin scripts do,
	// Notify the notification config and secrets, and IngestFailures where
	// ingestion failures are recorded. The server has no default for them.
	Tokens         func(db DB) token.Store
	Sources        func(db DB) source.Store
	Crons          func(db DB) cron.Store
	Bins           func(db DB) bin.Store
	Notify         func(db DB) notifycfg.Service
	IngestFailures func(db DB) ingestfail.Store

	// Mounts is the mounts the shell's filesystem is built over for a session.
	Mounts func(db DB) func(session weftapi.Session, logServices []string) mounts.Config

	// Entitlements is what each tenant's plan allows: the cron limits the api
	// reports and the retention it clamps a cron's chart to. Nil: no limits.
	Entitlements func(db DB) tenancy.Entitlements

	// AgentFleet is where the agents reading each source are recorded. Nil: none.
	AgentFleet func(db DB) sources.AgentFleet

	// AlertTargets are the alert destination kinds the deployment sends beyond
	// the core's (webhook, email). The api only checks /etc/notify.yaml
	// against them when it is saved; the coordinator sends. Nil: the core's.
	AlertTargets func(db DB) []alerting.AlertTarget

	// Producers adds the deployment's own log producers to the registry the api
	// serves (the core's are Heroku drains, Firehose delivery from CloudWatch,
	// OTLP metrics and Render's TLS syslog). A producer that implements an HTTPS or
	// syslog transport interface is mounted or listened on like the core's.
	Producers func(*sources.Registry)

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

// validate is why a process whose adapter lacks a required part does not
// start: the server has no default identity or tenancy to fall back on.
func (a Adapter[DB]) validate() error {
	for name, set := range map[string]bool{
		"Open": a.Open != nil, "Identity": a.Identity != nil, "Tenancy": a.Tenancy != nil,
	} {
		if !set {
			return fmt.Errorf("the adapter has no %s: the api has no default for it", name)
		}
	}
	return nil
}
