// Package tenancy is the seam between weft's query path and whoever decides
// what a principal may see. See docs/plans/tenant-scope.md.
package tenancy

import (
	"context"
	"errors"

	"github.com/graybeardcorp/weft/shape"
)

// Actor is a user acting within a partition. The Scoper that issued it is the
// only code that looks inside.
type Actor interface {
	// Partition is the tenant whose data the actor is working in. It is the
	// value of the required customer column in every shape.Coord.
	Partition() int64
}

// Scoper decides data visibility.
type Scoper interface {
	// ReadScope is what the actor may read right now. It is called per
	// request and never cached across requests. Any error means deny: there
	// is no fallback scope.
	ReadScope(ctx context.Context, a Actor) (shape.Set, error)
}

// Action is something a principal can ask to change. The contract's set is
// closed and covers what the core does to data; an Authorizer denies any
// action it does not know, so a deployment without a product surface (teams,
// billing, a platform operator) is denied it by default. Adapters may define
// actions of their own for their own surfaces, as hosted does.
type Action string

const (
	ActionCreateNamespace       Action = "create-namespace"
	ActionDeleteNamespace       Action = "delete-namespace"
	ActionManageNamespaceTokens Action = "manage-namespace-tokens" // tokens and sources of Resource.Namespace
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
