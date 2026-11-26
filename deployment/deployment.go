// Package deployment is what the api and the coordinator share: the
// credential, the stores, the access policy, the limits and the alert
// destinations of one deployment, built once.
//
// The core defines the shape and never builds one. How a deployment keeps its
// data (SQLite and git on this machine, Postgres, a service that takes writes
// over HTTPS) is the deployer's choice: they fill the interfaces. Package
// deployment/local is the open build's choice.
package deployment

import (
	"errors"
	"fmt"
	"strings"

	"github.com/w3ft-net/weft/alerting"
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/planner"
)

// Deployment is everything the api and the coordinator read the same way.
type Deployment struct {
	// Authn is how callers are authenticated: one credential, answered once for
	// the api's HTTP requests and once for the coordinator's gRPC calls. Each
	// process needs its own half.
	Authn Authn
	// Access is how a user becomes a principal, what the principal may change
	// (Authz) and what it may read (Scope). Required, all three parts.
	Access identity.Access
	// Entitlements is what each group's limits are. Nil: no limits.
	Entitlements identity.Entitlements
	// Backends is where the deployment keeps what it serves. Required.
	Backends Backends
	// Mounts is what the shell's filesystem is built over. Required.
	Mounts *mounts.Backends
	// AlertTargets are the alert destination kinds the deployment sends beyond
	// the core's. The api checks /etc/notify.yaml against them when it is
	// saved; the coordinator sends.
	AlertTargets []alerting.AlertTarget
	// Dialects are the SQL dialects of the deployment's live stores and of its
	// archive, which the api and the coordinator plan queries in. A tier left
	// nil is DuckDB, so a deployment on the open build's stores says nothing.
	// Each must be the dialect of what the shard nodes run and the archive
	// reads: a plan is written for one.
	Dialects planner.Dialects
}

// Authn is the one credential's two faces.
type Authn struct {
	// API authenticates the api's HTTP callers and registers its own sign-in
	// and account routes.
	API weftapi.Identity
	// GRPC authenticates the callers of the coordinator's gRPC servers.
	GRPC identity.Server
}

// Backends is where the stores live, as every process reads them.
type Backends struct {
	Tokens  token.Store
	Sources source.Store
	Crons   cron.Store
	Bins    bin.Store
	// Dashboards is /etc/dashboards. Nil serves no dashboard routes.
	Dashboards     dashboard.Store
	Notify         notifycfg.Service
	IngestFailures ingestfail.Store
}

// Validate names what the deployment lacks of what every process needs, which
// is everything but Authn: each process checks the half it uses.
func (d *Deployment) Validate() error {
	if d == nil {
		return errors.New("the deployment is missing")
	}
	a := d.Access
	var missing []string
	for _, p := range []struct {
		name string
		set  bool
	}{
		{"Access", a.PrincipalOf != nil && a.Authz != nil && a.Scope != nil},
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
		return fmt.Errorf("the deployment lacks %s: there is no default for them", strings.Join(missing, ", "))
	}
	return nil
}
