// Package single is the adapter for a build with one tenant: no organizations,
// no teams, no permission tables. Every actor reads everything, and the closed
// set of actions the core defines is allowed. It is the default shape's
// adapter (docs/plans/tenant-scope.md, Phase 4); under a shape with required
// columns its actor names no tenant, and the planner refuses its scope.
package single

import (
	"context"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// Actor is a user of the one tenant. An empty User is the system identity.
type Actor struct{ User string }

func (Actor) Partition() shape.Added { return shape.Added{} }
func (a Actor) Subject() string      { return a.User }

// Scoper reads the whole of the actor's partition, which for a build with one
// tenant is everything.
type Scoper struct{}

func (Scoper) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
	if a == nil {
		return nil, tenancy.Denied("no actor")
	}
	return shape.Set{a.Partition().Coord()}, nil
}

// NamespaceScope is what a namespace's crons may read: with no groups to
// narrow it, the whole partition.
func (Scoper) NamespaceScope(_ context.Context, partition shape.Added, _ string) (shape.Set, error) {
	return shape.Set{partition.Coord()}, nil
}

// Authorizer allows the actions the core defines and denies any other: a
// build without a product surface has nothing else to allow. Under a shape
// that has a tenant it denies everything. Its actors name no tenant, so
// reaching it there means the build forgot its own adapter, and an open
// authorizer would then allow writes the real one gates.
type Authorizer struct{}

func (Authorizer) Authorize(_ context.Context, _ tenancy.Actor, act tenancy.Action, _ tenancy.Resource) error {
	if len(shape.BuildLayout().Columns) > 0 {
		return tenancy.Denied("this build has tenants and no adapter that knows them")
	}
	switch act {
	case tenancy.ActionCreateNamespace, tenancy.ActionDeleteNamespace, tenancy.ActionManageNamespaceTokens,
		tenancy.ActionManageBin, tenancy.ActionManageEtc, tenancy.ActionManageCron,
		tenancy.ActionPublishOrgWide, tenancy.ActionGrantNamespaceRead:
		return nil
	}
	return tenancy.Denied("not available")
}

// Orgs has no organizations: every partition is org 0, which the store lookups
// that key by org treat as "not asked".
type Orgs struct{}

func (Orgs) OrgOf(shape.Added) store.OrgID                    { return 0 }
func (Orgs) AddedOf(store.OrgID) shape.Added                  { return shape.Added{} }
func (Orgs) ActorOf(_ store.OrgID, user string) tenancy.Actor { return Actor{User: user} }
