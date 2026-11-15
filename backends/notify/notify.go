// Package notify is the contract of the notification-configuration domain:
// the document that names where alerts go (/etc/notify.yaml) and the named
// secrets its webhook targets may refer to. Postgres implements it for the hosted
// build and SQLite for the open one (see defaults and
// notify/notifystoretest).
//
// The contract holds what is stored, not what it may say: whether the document
// parses, keeps its system alias and refers only to secrets that exist is
// notifycfg.Service's to judge before it writes.
//
// It imports only the standard library and shape.
package notify

import (
	"errors"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a secret, or a document to amend, that does not
// exist.
var ErrNotFound = errors.New("not found")

// ErrNotYours is the answer to an amendment restricted to the author when the
// current version was written by someone else.
var ErrNotYours = errors.New("the current version was written by someone else")

// Document is the notify.yaml an owner has saved. The zero value is one that was
// never saved.
type Document struct {
	Text      string
	UpdatedAt time.Time
}

// Store is the persistence contract for the notify.yaml document. Every
// operation is confined to its owner.
type Store interface {
	// Aliases is the owner's notify.yaml, the zero Document if it has never been
	// saved.
	Aliases(owner shape.Added) (Document, error)
	// SaveAliases writes the text as given; user is who wrote it, "" for none.
	// Saving the text it already has is a no-op.
	SaveAliases(owner shape.Added, user, text string) error
	// AmendMessage sets the commit message of the current version. A non-empty
	// authorID restricts it to a version that user wrote, ErrNotYours otherwise.
	// ErrNotFound if the document has never been saved.
	AmendMessage(owner shape.Added, authorID, message string) error
}

// Secrets is the persistence contract for the named secrets webhook targets
// refer to as ${secrets.NAME}. A value is written and read by the server, and
// never shown to a user: only names are listed.
type Secrets interface {
	// Names lists the owner's secret names, sorted.
	Names(owner shape.Added) ([]string, error)
	// Value is a secret's value, ErrNotFound if there is none of that name.
	Value(owner shape.Added, name string) (string, error)
	// Set creates the secret or replaces its value.
	Set(owner shape.Added, name, value string) error
	// Delete removes the secret outright, so that its value does not linger.
	// ErrNotFound if there is none of that name.
	Delete(owner shape.Added, name string) error
}
