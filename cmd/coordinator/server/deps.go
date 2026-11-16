// Package server is the coordinator process: flags, listeners, the store, the
// shard control plane, the gRPC servers and their shutdown. It is everything
// that is the same in every deployment, and it names none of the things that
// differ. A deployment's main supplies those as the Deps its open function
// returns:
//
//	func main() { server.Run(open) }
package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/mounts"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/coordinator"
	coordsvc "github.com/w3ft-net/weft/coordinator/service"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/logdb"
)

// Deps is everything a deployment supplies, already built over an open
// database. A field marked optional left nil is the open
// default: no feature, or the one-group behaviour.
type Deps struct {
	// Identity is who the groups are, who calls, and what each may read and
	// change. Required.
	Identity Identity
	// Backends is where the coordinator's data lives. Required, except where a
	// member says otherwise.
	Backends Backends
	// Notifier is the alert notifier, built over the store so it can
	// pre-resolve aliases. Required.
	Notifier *coordinator.Notifier
	// Mounts is what the shell's mounts read and write through. Required.
	Mounts *mounts.Backends
	// ColdScans meters cold-storage reads against the plan's cap. Optional:
	// they are not metered.
	ColdScans *coordinator.ColdScanLedger
	// Commands registers the deployment's own shell commands. Optional.
	Commands coordinator.CommandsFunc
	// Retention is the retention feature for the shard control plane. Optional:
	// archives are kept until an operator removes them.
	Retention logdb.Retention
	// LegacyFactTable names the table of field facts from shards that predate
	// table names. Optional: such facts are unknown.
	LegacyFactTable logdb.LegacyFactTable
	// Extension is the deployment's own behaviour beside the core's. Nil: none.
	Extension Extension
}

// Identity is everything the deployment says about who acts.
type Identity struct {
	// Authn opens, once and where the process reads its secrets, the server
	// that authenticates the callers of the gRPC servers, so a missing secret is
	// reported once and at the same point on every start. A coordinator with no
	// way to authenticate its callers must not start. Required.
	Authn identity.Server
	// Orgs maps a group's org handle to the shape. Required.
	Orgs coordinator.Orgs
	// Scope is what a principal may read and Authz what it may change. The gRPC
	// service takes its Authorizer from the same config, so a gate the service
	// applies is never looser than one the coordinator applies. Required.
	Scope identity.Scoper
	Authz identity.Authorizer
	// Entitlements is what each group's plan allows. Optional: no limits.
	Entitlements identity.Entitlements
}

// Backends is where the coordinator's data lives.
type Backends struct {
	// Tokens is where agent tokens live, Sources where namespaces and sources
	// do and Notices where user notices do.
	Tokens  token.Store
	Sources source.Store
	Notices notice.Store
	// Crons is where the crons the coordinator runs live: watches, what they
	// recorded and their alert incidents. Bins is where /bin scripts live.
	Crons cron.Store
	Bins  bin.Store
	// IngestFailures is where the drain records the batches it could not place,
	// which the api reads back as the services a group should look at. A
	// coordinator without it drops every failure it reports.
	IngestFailures ingestfail.Store
	// Coordination is how this coordinator knows it may act: the leases that
	// elect its leaders and the claims that keep a cron slot from running twice.
	Coordination coordination.Coordination
	// Shards is where shard state lives: shard rows, buckets and heartbeats.
	// Services is the sources registry the provisioner asks which services a
	// bucket serves. Standbys is the warm pool's rows. Optional: none.
	Shards   logdb.ShardStore
	Services logdb.ServiceDirectory
	Standbys logdb.StandbyStore
	// DeadLetters is where a batch the coordinator cannot place on a shard goes,
	// and where the replay loop reads it back from. Optional: such batches are
	// dropped.
	DeadLetters dlq.Store
	// AgentSeen batches the token last-seen stamps the service writes on every
	// stream open, flushed by Run until the process stops. Optional: dropped.
	AgentSeen AgentSeen
}

// AgentSeen records when an agent's token was last seen, without a database
// write per stream open.
type AgentSeen interface {
	Touch(tokenID uint, clientID string, at time.Time)
	Run(ctx context.Context, interval time.Duration)
}

// Extension is the deployment's own behaviour on the running process (hosted:
// the subscription an agent must hold, usage metering, dev seeding). Embed
// NopExtension and override what is needed.
type Extension interface {
	// ConfigureService sets what the gRPC service needs of the deployment: the
	// subscription an agent must hold, the usage it reports, the names of its
	// orgs. Without it the service admits every group and reports none of that.
	ConfigureService(srv *coordsvc.Server)
	// Start runs the deployment's own startup work, once the shard control
	// plane is up and before the crons start.
	Start(rt Runtime)
}

// NopExtension does nothing. Embed it in an Extension to override only some.
type NopExtension struct{}

func (NopExtension) ConfigureService(*coordsvc.Server) {}
func (NopExtension) Start(Runtime)                     {}

// Runtime is what Extension.Start gets of the running process.
type Runtime struct {
	Coord *coordinator.Coordinator
	// Ctx lives as long as the coordinator's drain loop does.
	Ctx context.Context
	// Customer is the --customer flag: a fallback group for dev and docker.
	Customer string

	// Holder identifies this process in the lease table, and Leases is where
	// leases are taken. AWSRegion is the --aws-region flag, and InstanceID the
	// EC2 instance id (empty off EC2).
	Holder     string
	Leases     coordination.Leases
	AWSRegion  string
	InstanceID string
	// AwaitOnExit registers background work whose end the process waits for
	// (for at most a few seconds) when it stops, after Ctx is cancelled: an
	// election that gives its lease back, say. done is closed when it has ended.
	AwaitOnExit func(done <-chan struct{})
}

// Validate is why a process whose deployment lacks a required part does not
// start: the coordinator has no default for any of them.
func (d *Deps) Validate() error {
	if d == nil {
		return errors.New("the deployment supplied no Deps")
	}
	b := d.Backends
	var missing []string
	for _, p := range []struct {
		name string
		set  bool
	}{
		{"Identity.Authn", d.Identity.Authn != nil},
		{"Identity.Orgs", d.Identity.Orgs != nil},
		{"Identity.Scope", d.Identity.Scope != nil},
		{"Identity.Authz", d.Identity.Authz != nil},
		{"Backends.Tokens", b.Tokens != nil},
		{"Backends.Sources", b.Sources != nil},
		{"Backends.Notices", b.Notices != nil},
		{"Backends.Crons", b.Crons != nil},
		{"Backends.Bins", b.Bins != nil},
		{"Backends.IngestFailures", b.IngestFailures != nil},
		{"Backends.Coordination", b.Coordination != nil},
		{"Backends.Shards", b.Shards != nil},
		{"Backends.Services", b.Services != nil},
		{"Notifier", d.Notifier != nil},
		{"Mounts", d.Mounts != nil},
	} {
		if !p.set {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the deployment lacks %s: the coordinator has no default for them", strings.Join(missing, ", "))
	}
	return nil
}

// CoordinatorConfig fills cfg with what the deployment says about its groups,
// over its backends.
func (d *Deps) CoordinatorConfig(cfg coordinator.CoordinatorConfig) coordinator.CoordinatorConfig {
	b := d.Backends
	cfg.Orgs = d.Identity.Orgs
	cfg.Scoper = d.Identity.Scope
	cfg.Authz = d.Identity.Authz
	cfg.Entitlements = d.Identity.Entitlements
	cfg.Crons = b.Crons
	cfg.Bins = b.Bins
	cfg.Sources = b.Sources
	cfg.Notices = b.Notices
	cfg.Shards = b.Shards
	cfg.Services = b.Services
	cfg.Errors = coordinator.NewIngestionErrorRecorder(b.IngestFailures)
	cfg.Mounts = d.Mounts
	cfg.ColdScans = d.ColdScans
	cfg.Commands = d.Commands
	return cfg
}
