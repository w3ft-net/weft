// Package server is the HTTP api process, split from cmd/api the way
// cmd/coordinator/server is split from cmd/coordinator: flags, the database,
// the coordinator connection, the syslog listener and the server are the same
// everywhere, and what differs (who the users are and the product's own
// configuration) comes from the Deps a deployment's open function returns.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/w3ft-net/weft/alerting"
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// Deps is what a deployment supplies, already built. Open returns it once the
// database is open; the server has no default for any required part.
type Deps struct {
	// Identity is who the api's users are and what they may do. Required.
	Identity Identity
	// Backends is where the api keeps what it serves. Required, except where
	// a member says otherwise.
	Backends Backends
	// Mounts is what the shell's filesystem is built over. Required.
	Mounts *mounts.Backends
	// Extension is the deployment's own behaviour beside the core's. Nil: none.
	Extension Extension
}

// Identity is everything the deployment says about who acts.
type Identity struct {
	// Authn authenticates the api's users and registers its own sign-in and
	// account routes. Required.
	Authn weftapi.Identity
	// Access is how a user becomes a principal, what the principal may change
	// (Authz) and what it may read (Scope). Required, all three parts.
	Access identity.Access
	// Groups says whether a partition names a group; the routes that read or
	// write a group's data refuse a session that acts in none. Nil: every
	// partition does.
	Groups func(shape.Added) bool
	// Entitlements is what each group's plan allows: the cron limits the api
	// reports and the retention it clamps a cron's chart to. Nil: no limits.
	Entitlements identity.Entitlements
}

// Backends is where the api's data lives.
type Backends struct {
	Tokens         token.Store
	Sources        source.Store
	Crons          cron.Store
	Bins           bin.Store
	Notify         notifycfg.Service
	IngestFailures ingestfail.Store
	// AgentFleet is where the agents reading each source are recorded. Optional:
	// nil, and no source reports agents.
	AgentFleet sources.AgentFleet
}

// Extension is the deployment's own behaviour on the server (hosted: billing,
// the operator's admin, integrations). Embed NopExtension and override what is
// needed.
type Extension interface {
	// AlertTargets are the alert destination kinds the deployment sends beyond
	// the core's (webhook, email). The api only checks /etc/notify.yaml against
	// them when it is saved; the coordinator sends.
	AlertTargets() []alerting.AlertTarget
	// Producers adds the deployment's own log producers to the registry the api
	// serves (the core's are Heroku drains, Firehose delivery from CloudWatch,
	// OTLP metrics and Render's TLS syslog). A producer that implements an HTTPS
	// or syslog transport interface is mounted or listened on like the core's.
	Producers(*sources.Registry)
	// Routes adds the deployment's own routes to the api's mux.
	Routes(*weftapi.Routes)
	// Configure sets the deployment's own configuration on the server, once it
	// is connected to the coordinator (hosted: Stripe prices and keys, Slack
	// client).
	Configure(*weftapi.Server)
	// Start begins the deployment's background work, before the syslog
	// listener (hosted: the sweep of accounts that never verified an email).
	Start(ctx context.Context, srv *weftapi.Server)
	// Serving begins work that belongs beside the running listeners (hosted:
	// the daily Stripe usage reporter).
	Serving(ctx context.Context, srv *weftapi.Server)
}

// NopExtension does nothing. Embed it in an Extension to override only some.
type NopExtension struct{}

func (NopExtension) AlertTargets() []alerting.AlertTarget     { return nil }
func (NopExtension) Producers(*sources.Registry)              {}
func (NopExtension) Routes(*weftapi.Routes)                   {}
func (NopExtension) Configure(*weftapi.Server)                {}
func (NopExtension) Start(context.Context, *weftapi.Server)   {}
func (NopExtension) Serving(context.Context, *weftapi.Server) {}

// Validate is why a process whose deployment lacks a required part does not
// start: the server has no default identity, access or backends to fall back
// on.
func (d *Deps) Validate() error {
	if d == nil {
		return errors.New("the deployment supplied no Deps")
	}
	a := d.Identity.Access
	var missing []string
	for _, p := range []struct {
		name string
		set  bool
	}{
		{"Identity.Authn", d.Identity.Authn != nil},
		{"Identity.Access", a.PrincipalOf != nil && a.Authz != nil && a.Scope != nil},
		{"Backends.Tokens", d.Backends.Tokens != nil},
		{"Backends.Sources", d.Backends.Sources != nil},
		{"Backends.Crons", d.Backends.Crons != nil},
		{"Backends.Bins", d.Backends.Bins != nil},
		{"Backends.IngestFailures", d.Backends.IngestFailures != nil},
		{"Mounts", d.Mounts != nil},
	} {
		if !p.set {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the deployment lacks %s: the api has no default for them", strings.Join(missing, ", "))
	}
	return nil
}
