// Package notice is the contract of the user-notice domain: the queue of
// one-time notices addressed to a user, left by whatever raised them and drained
// when the user is next served. Postgres implements it for the hosted build and
// SQLite for the open one (see defaults and notice/noticestoretest).
//
// A notice is a delivery, not a suppression ledger: one record per instance,
// pending until it is marked seen. There is deliberately no uniqueness by key and
// no "has this user seen X": a notice that shouldn't fire twice is one whose
// emitter shouldn't emit it twice. The prose lives with the renderer (package
// notices), because the browser and the shell render differently; a record holds
// only the facts.
//
// A user is named by id, which is unique across owners, so no operation takes
// one. It imports only the standard library.
package notice

import (
	"errors"
	"time"
)

// The kinds of notice, as the renderer's frontend decides how to show them.
const (
	KindSuccess = "success"
	KindWarning = "warning"
	KindError   = "error"
)

// ErrInvalidKind is the refusal to emit a notice of a kind that is not one of the
// constants above.
var ErrInvalidKind = errors.New("invalid notice kind")

// Pending is one undelivered notice instance.
type Pending struct {
	ID  int64
	Key string
	// Kind is one of the Kind constants.
	Kind string
	// Params are the facts a renderer interpolates. A value read back is what
	// encoding/json makes of it: lists are []any, numbers float64.
	Params    map[string]any
	CreatedAt time.Time
}

// Store is the persistence contract of the user-notice domain.
type Store interface {
	// Emit queues a notice for the user, ErrInvalidKind for a kind that is not
	// known. params may be nil.
	Emit(userID, key, kind string, params map[string]any) error
	// Pending is the user's undelivered notices, oldest first. Reading does not
	// deliver: the caller marks what it actually rendered, so a dropped
	// connection costs a repeat rather than the only copy.
	Pending(userID string) ([]Pending, error)
	// MarkSeen stamps the instances delivered. It is by id, so draining one
	// instance never burns another that shares its key. An empty list, or an id
	// already seen, is not an error.
	MarkSeen(ids []int64) error
}
