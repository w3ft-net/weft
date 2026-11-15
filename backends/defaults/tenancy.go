// The open tenancy: a build with one tenant, no organizations, no teams, no
// permission tables. Every actor reads everything, and the closed set of actions
// the core defines is allowed. It is the default shape's tenancy; under a shape
// with required columns its actor names no tenant, and the planner refuses its
// scope.

package defaults

import (
	"context"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/tenancy"
)

// OpenActor is a user of the one tenant. An empty User is the system identity.
type OpenActor struct{ User string }

func (OpenActor) Partition() shape.Added { return shape.Added{} }
func (a OpenActor) Subject() string      { return a.User }

// OpenScoper reads the whole of the actor's partition, which for a build with one
// tenant is everything.
type OpenScoper struct{}

func (OpenScoper) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
	if a == nil {
		return nil, tenancy.Denied("no actor")
	}
	return shape.Set{a.Partition().Coord()}, nil
}

// NamespaceScope is what a namespace's crons may read: with no groups to
// narrow it, the whole partition.
func (OpenScoper) NamespaceScope(_ context.Context, partition shape.Added, _ string) (shape.Set, error) {
	return shape.Set{partition.Coord()}, nil
}

// OpenAuthorizer allows the actions the core defines and denies any other: a
// build without a product surface has nothing else to allow. Under a shape
// that has a tenant it denies everything. Its actors name no tenant, so
// reaching it there means the build forgot its own adapter, and an open
// authorizer would then allow writes the real one gates.
type OpenAuthorizer struct{}

func (OpenAuthorizer) Authorize(_ context.Context, _ tenancy.Actor, act tenancy.Action, _ tenancy.Resource) error {
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

// OpenTenancy is the open tenancy as the parts a filesystem mount or an api server
// acts under.
func OpenTenancy() tenancy.Adapter {
	return tenancy.Adapter{ActorOf: func(_ shape.Added, user string) tenancy.Actor { return OpenActor{User: user} }, Authz: OpenAuthorizer{}, Scope: OpenScoper{}}
}

// OpenOrgs has no organizations: an actor carries only its user.
type OpenOrgs struct{}

func (OpenOrgs) ActorOf(_ shape.Added, user string) tenancy.Actor { return OpenActor{User: user} }
