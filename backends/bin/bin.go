// Package bin is the contract of the /bin domain: the scripts a tenant keeps
// under /bin, which crons and the shell run by name. Postgres implements it for
// the hosted build and SQLite for the open one (see defaults and
// bin/binstoretest).
//
// The contract holds what is stored, not what a script may say: whether a source
// parses, whether it is safe to run from a cron and which crons run it are the
// callers' to judge. A script is named by the owner, a bare name and nothing
// else, so a language is a property of the script, never of the name.
//
// It imports only the standard library and shape.
package bin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a script the caller named that does not exist,
// or has been deleted.
var ErrNotFound = errors.New("script not found")

// ErrNotYours is the answer to an amendment restricted to the author when the
// current version was written by someone else.
var ErrNotYours = errors.New("the current version was written by someone else")

// The languages a script is written in.
const (
	LangBash = "bash"
	LangLua  = "lua"
)

// Script is a stored /bin script.
type Script struct {
	ID    uint
	Owner shape.Added
	Name  string
	// Source is the script as it runs: a Lua script has its shebang stripped.
	Source   string
	Language string
	// ContentHash is ContentHash(Source).
	ContentHash string
	// Message is the commit message of the current version.
	Message string
	// CreatedBy is the user who first saved the script, "" for none; an edit
	// keeps it. AuthoredBy is the user who wrote the current version.
	CreatedBy  string
	AuthoredBy string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ContentHash is the hash a script's source is stored under: SHA-256, in hex.
func ContentHash(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}

// Store is the persistence contract of the /bin domain. Every operation is
// confined to its owner's scripts.
type Store interface {
	// Save writes the script: a new one, an edit, or a deleted one brought back,
	// which then keeps its creator. Saving the source a script already has is a
	// no-op, so that an unchanged buffer written back leaves its version alone.
	// An edit sets AuthoredBy to user and clears Message. Language is LangBash or
	// LangLua.
	Save(owner shape.Added, user, name, source, language string) error
	// Get is the script, ErrNotFound if there is none.
	Get(owner shape.Added, name string) (*Script, error)
	// List is the owner's scripts by name.
	List(owner shape.Added) ([]Script, error)
	// Delete removes a script; a missing one is not an error.
	Delete(owner shape.Added, name string) error
	// AmendMessage sets the commit message of the script's current version. A
	// non-empty authorID restricts it to a version that user wrote, ErrNotYours
	// otherwise. ErrNotFound if there is no such script.
	AmendMessage(owner shape.Added, authorID, name, message string) error
}
