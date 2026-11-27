// Package server is the HTTP api process, split from cmd/api the way
// cmd/coordinator/server is split from cmd/coordinator: flags, the database,
// the coordinator connection, the syslog listener and the server are the same
// everywhere, and what differs comes from the deployment.Deployment a
// deployment's open function returns, and the Own it adds to it.
package server

import (
	"context"
	"errors"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/deployment"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// Own is what only the api asks of a deployment, beside the deployment.Deployment
// every process shares.
type Own struct {
	// Groups says whether a partition names a group; the routes that read or
	// write a group's data refuse a session that acts in none. Nil: every
	// partition does.
	Groups func(shape.Added) bool
	// AgentFleet is where the agents reading each source are recorded. Optional:
	// nil, and no source reports agents.
	AgentFleet sources.AgentFleet
	// Extension is the deployment's own behaviour beside the core's. Nil: none.
	Extension Extension
}

// Extension is the deployment's own behaviour on the server (hosted: billing,
// the operator's admin, integrations). Embed NopExtension and override what is
// needed.
type Extension interface {
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

func (NopExtension) Producers(*sources.Registry)              {}
func (NopExtension) Routes(*weftapi.Routes)                   {}
func (NopExtension) Configure(*weftapi.Server)                {}
func (NopExtension) Start(context.Context, *weftapi.Server)   {}
func (NopExtension) Serving(context.Context, *weftapi.Server) {}

// validate is why a process whose deployment lacks a required part does not
// start: the api has no default identity, access or backends to fall back on.
func validate(dep *deployment.Deployment) error {
	if err := dep.Validate(); err != nil {
		return err
	}
	if dep.Authn.API == nil {
		return errors.New("the deployment lacks Authn.API: the api has no default for it")
	}
	return nil
}

// validateURLs is why an api without its external origins does not start: the
// urls it hands users (magic links, ingest endpoints) have no sensible default.
func validateURLs(fl *Flags) error {
	if fl.BaseURL == "" {
		return errors.New("--base-url is required: the web app's external origin")
	}
	if fl.APIBaseURL == "" {
		return errors.New("--api-base-url is required: the api's own external origin")
	}
	return nil
}
