// Package login is the contract of the email sign-in domain: which addresses may
// sign in, and the single-use links that prove an address is the caller's.
//
// An address is allowed by being in the store's list of emails. Whether a link
// is worth sending is the identity's decision (it asks Allowed first); the store
// issues a link for any address it is given. The raw link token exists only in
// the email: the store holds its sha256, so a copy of the database cannot be
// used to sign in.
//
// The package imports only the standard library, so a process that links an
// implementation stays cgo-free.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Store is the sign-in domain's persistence.
type Store interface {
	// Allowed reports whether email may sign in. The address is compared
	// case-insensitively.
	Allowed(email string) (bool, error)
	// IssueLink makes a single-use token for email that is good for ttl, and
	// returns the raw token. A ttl that is not positive makes a token that is
	// already expired.
	IssueLink(email string, ttl time.Duration) (raw string, err error)
	// ConsumeLink spends the token. It returns the address the token was issued
	// to, and ok false for a token that is unknown, expired or already spent.
	// Two concurrent calls with one token succeed at most once.
	ConsumeLink(raw string) (email string, ok bool, err error)
}

// Normalize is the form an address is stored and compared in.
func Normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// NewRaw is a new link token: lnk_ and 32 random bytes in hex.
func NewRaw() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "lnk_" + hex.EncodeToString(b), nil
}

// Hash is what the store keeps of a raw token.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
