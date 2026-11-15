package coordinator

import (
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/tenancy"
)

// Orgs says who acts as a user of a tenant. The tenant is the shape's added
// columns, which is all the data path carries; what a deployment keys its own
// tables by is its own, and the registries it hands in translate. The adapter
// supplies it; this package never names a tenant column.
// docs/plans/tenant-scope.md, Phase 4.
type Orgs interface {
	// ActorOf is the actor that acts as user in the tenant the columns name. An
	// empty user is the system identity, which crons and internal callers use.
	ActorOf(owner shape.Added, user string) tenancy.Actor
}

// CommandsFunc registers the shell commands a deployment adds to a query's
// interpreter (hosted: `git` over its version rows, and `clipboard`): the
// tenant and user the query runs as, the interpreter's working directory, the
// tenancy its mounts act under, the base URL of share links and the query's
// window.
type CommandsFunc func(interp *bash.Interpreter, owner shape.Added, userID string, cwd func() string, t tenancy.Adapter, shareBase string, start, end time.Time)

// MountsFunc is the mounts a query's shell filesystem is built over.
type MountsFunc func(owner shape.Added, plan string, logServices []string) mounts.Config

// Mapper is the coordinator's Orgs.
func (c *Coordinator) Mapper() Orgs { return c.Orgs }

// NoTenant reports whether the columns leave a required column unset, so they
// name no tenant a query could be planned for. A shape with no required column
// has no such columns.
func NoTenant(a shape.Added) bool { return (shape.Set{a.Coord()}).Validate() != nil }

// mountTenancy is what the filesystem mounts act under for this coordinator.
func (c *Coordinator) mountTenancy() tenancy.Adapter {
	return tenancy.Adapter{ActorOf: c.Mapper().ActorOf, Authz: c.Authz, Scope: c.Scoper}
}
