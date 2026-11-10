// Package tenancy is the seam between weft's query path and whoever decides
// what a principal may see. See docs/plans/tenant-scope.md.
package tenancy

import (
	"context"
	"errors"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
)

// Actor is a user acting within a partition. The Scoper that issued it is the
// only code that looks inside.
type Actor interface {
	// Partition is the org whose data the actor is working in.
	Partition() store.OrgID
}

// Scoper decides data visibility.
type Scoper interface {
	// ReadScope is what the actor may read right now. It is called per
	// request and never cached across requests. Any error means deny: there
	// is no fallback scope.
	ReadScope(ctx context.Context, a Actor) (shape.Set, error)
}

// Action is something a principal can ask to change. The set is closed: an
// Authorizer denies any action it does not know.
type Action string

const (
	ActionManageTeam            Action = "manage-team"             // invite, remove, set member permissions
	ActionManageBilling         Action = "manage-billing"          // plan, payment method
	ActionCreateNamespace       Action = "create-namespace"        //
	ActionDeleteNamespace       Action = "delete-namespace"        //
	ActionManageNamespaceTokens Action = "manage-namespace-tokens" // tokens and sources of Resource.Namespace
	ActionSuperadmin            Action = "superadmin"              // platform operator surface
)

// Resource is what an action is aimed at, for the actions that have a target.
type Resource struct {
	Namespace string
}

// ErrDenied is wrapped by every refusal from an Authorizer.
var ErrDenied = errors.New("permission denied")

type denied string

func (d denied) Error() string        { return string(d) }
func (d denied) Is(target error) bool { return target == ErrDenied }

// Denied returns a refusal carrying msg, which is safe to show the caller.
func Denied(msg string) error { return denied(msg) }

// Authorizer decides actions (writes and administration). Any error means
// the action is not allowed.
type Authorizer interface {
	Authorize(ctx context.Context, a Actor, act Action, r Resource) error
}
