// Package server is the coordinator process: flags, listeners, the store, the
// shard control plane, the gRPC servers and their shutdown. It is everything
// that is the same in every deployment, and it names none of the things that
// differ. A deployment's main supplies those through an Adapter:
//
//	func main() { server.Run(myadapter.Adapter()) }
//
// Hosted weft's adapter is tenancy/hosted/hostedapp. docs/plans/tenant-scope.md,
// Phase 4.
package server

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/w3ft-net/weft/tenancy"
)

// Adapter is everything a deployment supplies. A nil field is the open
// default: no feature, or the one-tenant behaviour. Only Identity is required,
// because a coordinator with no way to authenticate its callers must not start.
type Adapter[DB any] struct {
	// Coordinator says who the tenants are and what they may read and change.
	// A field it leaves nil is the open build's.
	Coordinator Tenancy[DB]

	// ConfigureService sets what the gRPC service needs of the deployment (hosted:
	// the subscription an agent must hold, the usage it reports, the names of
	// its orgs), once the store is open. Nil: the service admits every tenant
	// and reports none of that.
	ConfigureService func(db DB, srv *coordsvc.Server)

	// Identity opens, once and where the process reads its secrets, the server
	// that authenticates the callers of the gRPC servers, so a missing secret is
	// reported once and at the same point on every start. Required.
	Identity func(db DB) identity.Server

	// Open opens the deployment's database from the DSN (--db-dsn, else DB_DSN,
	// else empty) and returns it with what closes it: hosted migrates and
	// connects Postgres, and picks its own default for an empty DSN. Required.
	Open func(dsn string) (db DB, closeDB func(), err error)

	// AgentSeen batches the token last-seen stamps the service writes on every
	// stream open, flushed by Run until the process stops. Nil: dropped.
	AgentSeen func(db DB) interface {
		Touch(tokenID uint, clientID string, at time.Time)
		Run(ctx context.Context, interval time.Duration)
	}

	// Coordination is how this coordinator knows it may act: the leases that
	// elect its leaders and the claims that keep a cron slot from running
	// twice. Required.
	Coordination func(db DB) coordination.Coordination

	// Shards is where shard state lives: shard rows, buckets, the services
	// shards serve and heartbeats. Standbys is the warm pool's rows (nil: none)
	// and Services the sources registry the provisioner asks which services a
	// bucket serves. Shards and Services are required.
	Shards   func(db DB) logdb.ShardStore
	Standbys func(db DB) logdb.StandbyStore
	Services func(db DB) logdb.ServiceDirectory

	// Crons is where the crons the coordinator runs live: watches, what they
	// recorded and their alert incidents. Bins is where /bin scripts live. The
	// coordinator has no default for either.
	Crons func(db DB) cron.Store
	Bins  func(db DB) bin.Store

	// IngestFailures is where the drain records the batches it could not place,
	// which the api reads back as the services a tenant should look at. Required:
	// a coordinator without it drops every failure it reports.
	IngestFailures func(db DB) ingestfail.Store

	// Tokens is where agent tokens live, Sources where namespaces and sources do
	// and Notices where user notices do. The coordinator has no default for them.
	Tokens  func(db DB) token.Store
	Sources func(db DB) source.Store
	Notices func(db DB) notice.Store

	// DeadLetters is where a batch the coordinator cannot place on a shard goes,
	// and where the replay loop reads it back from (region is the --region flag,
	// for a store that lives in a cloud). Nil: such batches are dropped.
	DeadLetters func(db DB, region string) dlq.Store

	// Notifier builds the alert notifier over the store. Nil: a notifier that
	// sends no email.
	Notifier func(db DB) *coordinator.Notifier

	// Retention builds the retention feature for the shard control plane
	// (region signs its Lambda calls). Nil: archives are kept until an
	// operator removes them.
	Retention func(db DB, region string) logdb.Retention

	// LegacyFactTable names the table of field facts from shards that predate
	// table names. Nil: such facts are unknown.
	LegacyFactTable logdb.LegacyFactTable

	// Start runs the deployment's own startup work, once the shard control
	// plane is up and before the crons start (hosted: usage metering, dev
	// seeding).
	Start func(Runtime[DB])
}

// Tenancy is what a deployment says about its tenants: who they are, what each
// may read and change, and what the coordinator adds to a query for them.
type Tenancy[DB any] struct {
	Orgs         coordinator.Orgs
	Entitlements func(DB) tenancy.Entitlements
	Scoper       func(DB) tenancy.Scoper
	Authorizer   func(DB) tenancy.Authorizer
	// Mounts is the shell's mounts for a tenant on a plan.
	Mounts func(DB) coordinator.MountsFunc
	// ColdScans meters cold-storage reads against the plan's cap. Nil: they are
	// not metered.
	ColdScans func(DB) *coordinator.ColdScanLedger
	// Commands registers the deployment's own shell commands.
	Commands func(DB) coordinator.CommandsFunc
}

// Runtime is what Adapter.Start gets of the running process.
type Runtime[DB any] struct {
	DB    DB
	Coord *coordinator.Coordinator
	// Ctx lives as long as the coordinator's drain loop does.
	Ctx context.Context
	// Customer is the --customer flag: a fallback tenant for dev and docker.
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

func (a Adapter[DB]) validate() error {
	if a.Identity == nil {
		return errors.New("the adapter has no Identity: a coordinator that cannot authenticate its callers does not start")
	}
	for name, set := range map[string]bool{
		"Tokens": a.Tokens != nil, "Sources": a.Sources != nil, "Notices": a.Notices != nil,
		"Crons": a.Crons != nil, "Bins": a.Bins != nil, "Notifier": a.Notifier != nil,
		"Coordinator.Mounts": a.Coordinator.Mounts != nil, "Open": a.Open != nil,
		"Coordinator.Orgs": a.Coordinator.Orgs != nil, "Coordinator.Scoper": a.Coordinator.Scoper != nil,
		"Coordinator.Authorizer": a.Coordinator.Authorizer != nil,
		"Coordination":           a.Coordination != nil, "Shards": a.Shards != nil, "Services": a.Services != nil,
		"IngestFailures": a.IngestFailures != nil,
	} {
		if !set {
			return fmt.Errorf("the adapter has no %s: the coordinator has no default for it", name)
		}
	}
	return nil
}

// WithTenancy fills cfg with what the deployment says about its tenants, over
// this store: how its org handle maps to the shape, what an actor may read and
// what it may change. The coordinator's service takes its Authorizer from the
// same config, so a gate the service applies is never looser than one the
// coordinator applies.
func (a Adapter[DB]) WithTenancy(db DB, cfg coordinator.CoordinatorConfig) coordinator.CoordinatorConfig {
	cfg.Orgs = a.Coordinator.Orgs
	cfg.Crons = a.Crons(db)
	cfg.Bins = a.Bins(db)
	cfg.Sources = a.Sources(db)
	cfg.Notices = a.Notices(db)
	cfg.Shards = a.Shards(db)
	cfg.Services = a.Services(db)
	cfg.Errors = coordinator.NewIngestionErrorRecorder(a.IngestFailures(db))
	if a.Coordinator.Mounts != nil {
		cfg.Mounts = a.Coordinator.Mounts(db)
	}
	if a.Coordinator.ColdScans != nil {
		cfg.ColdScans = a.Coordinator.ColdScans(db)
	}
	if a.Coordinator.Commands != nil {
		cfg.Commands = a.Coordinator.Commands(db)
	}
	if a.Coordinator.Entitlements != nil {
		cfg.Entitlements = a.Coordinator.Entitlements(db)
	}
	if a.Coordinator.Scoper != nil {
		cfg.Scoper = a.Coordinator.Scoper(db)
	}
	if a.Coordinator.Authorizer != nil {
		cfg.Authz = a.Coordinator.Authorizer(db)
	}
	return cfg
}

func (a Adapter[DB]) retention(db DB, region string) logdb.Retention {
	if a.Retention != nil {
		return a.Retention(db, region)
	}
	return nil
}

// deadLetters is the adapter's store, or none. A nil result is a true nil
// interface, which the coordinator reads as "drop".
func (a Adapter[DB]) deadLetters(db DB, region string) dlq.Store {
	if a.DeadLetters == nil {
		return nil
	}
	return a.DeadLetters(db, region)
}
