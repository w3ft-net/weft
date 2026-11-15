// Package tenancy is the seam between weft's query path and whoever decides
// what a principal may see. See docs/plans/tenant-scope.md.
package tenancy

import (
	"context"
	"errors"

	"github.com/w3ft-net/weft/shape"
)

// Adapter is what the filesystem mounts act under: how an org's user becomes an
// Actor, what that Actor may do, and what it may read. The mounts hold no
// default; one that is missing denies everything.
type Adapter struct {
	// ActorOf is the actor that acts as user in the tenant the columns name. An
	// empty user is the system identity, which crons and internal callers use.
	ActorOf func(owner shape.Added, user string) Actor
	Authz   Authorizer
	Scope   Scoper
}

// Actor is a user acting within a partition. The Scoper that issued it is the
// only code that looks inside.
type Actor interface {
	// Partition is the tenant whose data the actor is working in. It is the
	// value of the required customer column in every shape.Coord.
	Partition() shape.Added
	// Subject names who is acting, for attribution. Empty is the system
	// identity: crons and internal callers.
	Subject() string
}

type actorKey struct{}

// WithActor returns ctx carrying the actor a request runs as.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom is the actor WithActor bound to ctx.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok && a != nil
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
	ActionManageBin             Action = "manage-bin"              // change a /bin script; Resource.Namespace is one whose crons run it, empty for a script no cron runs
	ActionManageEtc             Action = "manage-etc"              // write /etc
	ActionManageCron            Action = "manage-cron"             // write a cron homed in Resource.Namespace
	ActionPublishOrgWide        Action = "publish-org-wide"        // write an org-level cron, whose output every member reads
	ActionGrantNamespaceRead    Action = "grant-namespace-read"    // let Resource.Namespace's crons read Resource.Target
)

// Resource is what an action is aimed at, for the actions that have a target.
type Resource struct {
	Namespace string
	// Target is a second namespace, for an action that connects two: the one
	// whose logs are being opened to Namespace's crons.
	Target string
}

// ErrDenied is wrapped by every refusal from an Authorizer.
var ErrDenied = errors.New("permission denied")

type denied string

func (d denied) Error() string        { return string(d) }
func (d denied) Is(target error) bool { return target == ErrDenied }

// Denied returns a refusal carrying msg, which is safe to show the caller.
func Denied(msg string) error { return denied(msg) }

// NamespaceScoper is the part of a Scoper that answers for a namespace's crons:
// what they may read right now, which is the namespace itself plus whatever has
// been opened to them. Called when a cron is saved, and at every fire.
type NamespaceScoper interface {
	NamespaceScope(ctx context.Context, partition shape.Added, namespace string) (shape.Set, error)
}

// Authorizer decides actions (writes and administration). Any error means
// the action is not allowed.
type Authorizer interface {
	Authorize(ctx context.Context, a Actor, act Action, r Resource) error
}

// Readable is the namespaces the actor may read, and whether that is every
// namespace in their tenant. existing lists the tenant's namespaces and is
// only called when the scope leaves the namespace unconstrained, so a name
// the scope does not mention is never granted. A scope narrower than a
// namespace is shape.ErrFinerThanNamespace: callers that can only enforce
// namespaces must refuse it rather than widen it.
func Readable(ctx context.Context, sc Scoper, a Actor, existing func() ([]string, error)) (names []string, all bool, err error) {
	set, err := sc.ReadScope(ctx, a)
	if err != nil {
		return nil, false, err
	}
	names, all, err = set.Namespaces(a.Partition().Coord())
	if err != nil || !all {
		return names, false, err
	}
	names, err = existing()
	return names, true, err
}
