// Package history is the contract of the version-history domain: the record of
// what a group's tracked files (/bin scripts, /cron.d entries, dashboards, the
// notify config) said over time, and the git repository the shell's `git`
// command reads it as.
//
// It differs from the state contracts. Those store what is current; this one is
// told what changed (Journal) and serves what that adds up to (Source). The live
// rows stay where they are. A build chooses where the record lives:
//
//   - the hosted build keeps it in the version rows it already has, so its
//     Journal does nothing and its Source is built from those rows;
//   - history/gitrepo keeps it in a real git repository the operator owns, so
//     they can clone it, read it with their own tools, and push it to a remote
//     with credentials the server's git is configured with. A remote is that
//     implementation's configuration, never part of this contract.
//
// Paths are the ones the shell shows, without the leading slash: "bin/scan",
// "cron.d/ops/nightly", "etc/dashboards/errors.yaml", "etc/notify.yaml".
package history

import (
	"context"
	"errors"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a path with no history to delete or find, and
// for a commit that does not exist.
var ErrNotFound = errors.New("no such path or commit")

// ErrPublished is the refusal to reword a commit that has left the server: a
// remote has it, so changing its message would rewrite shared history.
var ErrPublished = errors.New("the commit has been published")

// Identity is who a change is attributed to.
type Identity struct{ Name, Email string }

// Change is one saved version of a tracked file, or its deletion.
type Change struct {
	Path string
	// Content is the file as the shell shows it (a Lua script with its shebang,
	// a cron in its serialized form). Ignored for a deletion.
	Content string
	Deleted bool
	Author  Identity
	// Message is the commit message; empty is "Create <path>", "Update <path>"
	// or "Delete <path>".
	Message string
	// At is when the change was made; zero is now.
	At time.Time
}

// Journal is what the write path tells after a save or delete has succeeded.
type Journal interface {
	// Record adds the change to the history and returns its commit. Saving the
	// content a path already has is not a change: it returns the path's current
	// commit. Deleting a path with no history is ErrNotFound.
	Record(ctx context.Context, owner shape.Added, c Change) (gitcore.ObjectID, error)
	// Reword sets the message of a commit, ErrPublished if it has left the
	// server and ErrNotFound if there is no such commit. Rewording may change the
	// IDs of the commits after it: a caller holds a commit only until it next
	// writes.
	Reword(ctx context.Context, owner shape.Added, id gitcore.ObjectID, message string) error
}

// Source is what the shell's `git` reads.
type Source interface {
	// Repository is the history as a git repository.
	Repository(owner shape.Added) (gitcore.Repository, error)
	// Latest is the newest commit that touched the path, ErrNotFound if none
	// did.
	Latest(owner shape.Added, path string) (gitcore.ObjectID, error)
}

// Store is a build's whole history domain.
type Store interface {
	Journal
	Source
}

type messageKey struct{}

// WithMessage carries a commit message down a write path that has no argument
// for one (a file write through the mounts), for whatever records the change to
// use as Change.Message.
func WithMessage(ctx context.Context, message string) context.Context {
	return context.WithValue(ctx, messageKey{}, message)
}

// MessageFrom is the message WithMessage put in ctx, "" if none.
func MessageFrom(ctx context.Context) string {
	m, _ := ctx.Value(messageKey{}).(string)
	return m
}
