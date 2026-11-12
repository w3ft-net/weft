package coordinator

import (
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/single"
)

// Orgs maps between the hosted store's handle for a tenant (the org id its
// tables are keyed by) and the shape's added columns, which is all the data
// path carries. The adapter supplies it; this package never names a tenant
// column. docs/plans/tenant-scope.md, Phase 4.
type Orgs interface {
	// OrgOf is the store handle of the tenant the columns name. Zero means no
	// org: a build whose shape has no tenant, or a tenant the store does not
	// know.
	OrgOf(shape.Added) store.OrgID
	// AddedOf is the added columns of an org.
	AddedOf(store.OrgID) shape.Added
	// ActorOf is the actor that acts as user in org. An empty user is the
	// system identity, which crons and internal callers use.
	ActorOf(org store.OrgID, user string) tenancy.Actor
}

// Adapter is what a deployment supplies to say who its tenants are and what
// they may do.
type Adapter struct {
	Orgs       Orgs
	Scoper     func(*store.Store) tenancy.Scoper
	Authorizer func(*store.Store) tenancy.Authorizer
}

// DefaultAdapter fills what a Coordinator or a service.Server is built without.
// It is the open adapter of a build with one tenant. A binary of another
// deployment passes its own adapter explicitly; tests of a hosted build install
// the hosted one from an init in a _test.go file.
var DefaultAdapter = Adapter{
	Orgs:       single.Orgs{},
	Scoper:     func(*store.Store) tenancy.Scoper { return single.Scoper{} },
	Authorizer: func(*store.Store) tenancy.Authorizer { return single.Authorizer{} },
}

// Mapper is the coordinator's Orgs, or the default's when it was built without
// one.
func (c *Coordinator) Mapper() Orgs {
	if c.Orgs != nil {
		return c.Orgs
	}
	return DefaultAdapter.Orgs
}

// noTenant reports whether the columns leave a required column unset, so they
// name no tenant a query could be planned for. A shape with no required column
// has no such columns.
func noTenant(a shape.Added) bool { return (shape.Set{a.Coord()}).Validate() != nil }

// mountTenancy is what the filesystem mounts act under for this coordinator.
func (c *Coordinator) mountTenancy() tenancy.Adapter {
	return tenancy.Adapter{ActorOf: c.Mapper().ActorOf, Authz: c.Authz, Scope: c.Scoper}
}
