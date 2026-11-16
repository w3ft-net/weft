// The open access: a build with one group, no organizations, no teams, no
// permission tables. Every principal reads everything, and the closed set of actions
// the core defines is allowed. It is the default shape's access; under a shape
// with required columns its principal names no group, and the planner refuses its
// scope.

package defaults

import (
	"context"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
)

// OpenPrincipal is a user of the one group. An empty User is the system identity.
type OpenPrincipal struct{ User string }

func (OpenPrincipal) Partition() shape.Added { return shape.Added{} }
func (a OpenPrincipal) Subject() string      { return a.User }

// OpenScoper reads the whole of the principal's partition, which for a build with one
// group is everything.
type OpenScoper struct{}

func (OpenScoper) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	if a == nil {
		return nil, identity.Denied("no principal")
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
// that has a group it denies everything. Its principals name no group, so
// reaching it there means the build forgot its own adapter, and an open
// authorizer would then allow writes the real one gates.
type OpenAuthorizer struct{}

func (OpenAuthorizer) Authorize(_ context.Context, _ identity.Principal, act identity.Action, _ identity.Resource) error {
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

// OpenAccess is the open access as the parts a filesystem mount or an api server
// acts under.
func OpenAccess() identity.Access {
	return identity.Access{PrincipalOf: func(_ shape.Added, user string) identity.Principal { return OpenPrincipal{User: user} }, Authz: OpenAuthorizer{}, Scope: OpenScoper{}}
}

// OpenOrgs has no organizations: a principal carries only its user.
type OpenOrgs struct{}

func (OpenOrgs) PrincipalOf(_ shape.Added, user string) identity.Principal {
	return OpenPrincipal{User: user}
}
