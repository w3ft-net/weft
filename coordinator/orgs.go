package coordinator

import (
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
)

// Orgs says who acts as a user of a group. The group is the shape's added
// columns, which is all the data path carries; what a deployment keys its own
// tables by is its own, and the registries it hands in translate. The adapter
// supplies it; this package never names a group column.
type Orgs interface {
	// PrincipalOf is the principal that acts as user in the group the columns name. An
	// empty user is the system identity, which crons and internal callers use.
	PrincipalOf(owner shape.Added, user string) identity.Principal
}

// CommandsFunc registers the shell commands a deployment adds to a query's
// interpreter (hosted: `git` over its version rows, and `clipboard`): the
// group and user the query runs as, the interpreter's working directory, the
// access its mounts act under, the base URL of share links and the query's
// window.
type CommandsFunc func(interp *bash.Interpreter, owner shape.Added, userID string, cwd func() string, t identity.Access, shareBase string, start, end time.Time)

// Mapper is the coordinator's Orgs.
func (c *Coordinator) Mapper() Orgs { return c.Orgs }

// NoGroup reports whether the columns leave a required column unset, so they
// name no group a query could be planned for. A shape with no required column
// has no such columns.
func NoGroup(a shape.Added) bool { return (shape.Set{a.Coord()}).Validate() != nil }

// mountAccess is what the filesystem mounts act under for this coordinator.
func (c *Coordinator) mountAccess() identity.Access {
	return identity.Access{PrincipalOf: c.Mapper().PrincipalOf, Authz: c.Authz, Scope: c.Scoper}
}
