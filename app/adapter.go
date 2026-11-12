// Package app is the coordinator process: flags, listeners, the store, the
// shard control plane, the gRPC servers and their shutdown. It is everything
// that is the same in every deployment, and it names none of the things that
// differ. A deployment's main supplies those through an Adapter:
//
//	func main() { app.Coordinator(myadapter.Adapter()) }
//
// Hosted weft's adapter is tenancy/hosted/hostedapp. docs/plans/tenant-scope.md,
// Phase 4.
package app

import (
	"context"
	"errors"

	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/identity"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/store"
)

// Adapter is everything a deployment supplies. A nil field is the open
// default: no feature, or the one-tenant behaviour. Only Identity is required,
// because a coordinator with no way to authenticate its callers must not start.
type Adapter struct {
	// Coordinator says who the tenants are and what they may read and change.
	// A field it leaves nil is coordinator.DefaultAdapter's.
	Coordinator coordinator.Adapter

	// Identity authenticates the callers of the gRPC servers. Required.
	Identity identity.Identity

	// Notifier builds the alert notifier over the store. Nil: a notifier that
	// sends no email.
	Notifier func(db *store.Store) *coordinator.Notifier

	// Retention builds the retention feature for the shard control plane
	// (region signs its Lambda calls). Nil: archives are kept until an
	// operator removes them.
	Retention func(db *store.Store, region string) logdb.Retention

	// LegacyFactTable names the table of field facts from shards that predate
	// table names. Nil: such facts are unknown.
	LegacyFactTable logdb.LegacyFactTable

	// Start runs the deployment's own startup work, once the shard control
	// plane is up and before the crons start (hosted: usage metering, dev
	// seeding).
	Start func(Runtime)
}

// Runtime is what Adapter.Start gets of the running process.
type Runtime struct {
	DB    *store.Store
	Coord *coordinator.Coordinator
	// Ctx lives as long as the coordinator's drain loop does.
	Ctx context.Context
	// Customer is the --customer flag: a fallback tenant for dev and docker.
	Customer string
}

func (a Adapter) validate() error {
	if a.Identity == nil {
		return errors.New("the adapter has no Identity: a coordinator that cannot authenticate its callers does not start")
	}
	return nil
}

// WithTenancy fills cfg with what the deployment says about its tenants, over
// this store: how its org handle maps to the shape, what an actor may read and
// what it may change. The coordinator's service takes its Authorizer from the
// same config, so a gate the service applies is never looser than one the
// coordinator applies. A part the adapter leaves nil stays nil, which
// NewCoordinator fills from coordinator.DefaultAdapter (the open one).
func (a Adapter) WithTenancy(db *store.Store, cfg coordinator.CoordinatorConfig) coordinator.CoordinatorConfig {
	cfg.Orgs = a.Coordinator.Orgs
	if a.Coordinator.Scoper != nil {
		cfg.Scoper = a.Coordinator.Scoper(db)
	}
	if a.Coordinator.Authorizer != nil {
		cfg.Authz = a.Coordinator.Authorizer(db)
	}
	return cfg
}

func (a Adapter) notifier(db *store.Store) *coordinator.Notifier {
	if a.Notifier != nil {
		return a.Notifier(db)
	}
	return coordinator.NewNotifier(db)
}

func (a Adapter) retention(db *store.Store, region string) logdb.Retention {
	if a.Retention != nil {
		return a.Retention(db, region)
	}
	return nil
}
