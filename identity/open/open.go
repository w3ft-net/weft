// Package open is the access policy of a build with one group: no organizations,
// no teams, no permission tables. Every principal reads everything, and the closed
// set of actions the core defines is allowed. It is the default shape's access;
// under a shape with required columns its principal names no group, and the
// planner refuses its scope.
package open

import (
	"context"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
)

// Principal is a user of the one group. An empty User is the system identity.
type Principal struct{ User string }

func (Principal) Partition() shape.Added { return shape.Added{} }
func (a Principal) Subject() string      { return a.User }

// Scoper reads the whole of the principal's partition, which for a build with one
// group is everything.
type Scoper struct{}

func (Scoper) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	if a == nil {
		return nil, identity.Denied("no principal")
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
// that has a group it denies everything. Its principals name no group, so
// reaching it there means the build forgot its own adapter, and an open
// authorizer would then allow writes the real one gates.
type Authorizer struct{}

func (Authorizer) Authorize(_ context.Context, _ identity.Principal, act identity.Action, _ identity.Resource) error {
	if len(shape.BuildLayout().Columns) > 0 {
		return identity.Denied("this build has groups and no adapter that knows them")
	}
	switch act {
	case identity.ActionCreateNamespace, identity.ActionDeleteNamespace, identity.ActionManageNamespaceTokens,
		identity.ActionManageBin, identity.ActionManageEtc, identity.ActionManageCron,
		identity.ActionPublishOrgWide, identity.ActionGrantNamespaceRead:
		return nil
	}
	return identity.Denied("not available")
}

// Access is the open access as the parts a filesystem mount or an api server
// acts under.
func Access() identity.Access {
	return identity.Access{PrincipalOf: func(_ shape.Added, user string) identity.Principal { return Principal{User: user} }, Authz: Authorizer{}, Scope: Scoper{}}
}

// Orgs has no organizations: a principal carries only its user.
type Orgs struct{}

func (Orgs) PrincipalOf(_ shape.Added, user string) identity.Principal {
	return Principal{User: user}
}
