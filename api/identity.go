package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/w3ft-net/weft/identity"
)

// Session is who a request acts as: the org its store calls are scoped by, the
// principal the Scoper and Authorizer judge, and the user. It is the Identity's to
// build; the core handlers read nothing else of it. A deployment's own fields
// (hosted: the active customer's name, the plan tier, the owner role) stay on
// its own type, which it reaches through the request context.
type Session = identity.Caller

// Identity is what a deployment supplies to authenticate the api's users. The
// core registers none of the routes a user signs in with and knows nothing of
// how they were authenticated: it asks the Identity to put a Session on each
// request, and to register whatever sign-in, token and account routes the
// deployment has. Namespace tokens (agents, producers, readers) are not the
// Identity's: they authenticate a namespace, not a user, and are the core's.
type Identity interface {
	// Authenticate wraps next so that it runs only for a request whose caller
	// it has resolved and bound to the context (identity.WithCaller), and
	// answers every other request itself.
	Authenticate(next http.Handler) http.Handler

	// Protect wraps next in whatever extra the deployment requires before a
	// request may have external effects (hosted: a verified email). Identity
	// has already run.
	Protect(next http.Handler) http.Handler

	// Entitled reports whether the session may run the shell. A non-nil error
	// is the reason, shown to the caller with 402.
	Entitled(ctx context.Context, s Session) error

	// Forward is what the coordinator needs to authenticate this request's
	// caller itself: gRPC metadata, as key/value pairs.
	Forward(r *http.Request) map[string]string

	// Routes registers the identity's own routes: sign-in, tokens, the account.
	Routes(rt *Routes)
}

// SetIdentity installs the deployment's Identity. Until it is called the
// server authenticates nobody.
func (s *Server) SetIdentity(i Identity) { s.identity = i }

// GetSession is the Session the Identity bound to the request, or nil when the
// route was not behind Authenticate.
func GetSession(r *http.Request) Session { return identity.CallerFrom(r.Context()) }

// ErrNoIdentity is why a server with no Identity refuses every user.
var ErrNoIdentity = errors.New("this api has no identity: it authenticates nobody")

// noIdentity authenticates nobody. A server nothing installed an Identity on
// refuses every user-facing request rather than serving it unauthenticated.
type noIdentity struct{}

func (noIdentity) Authenticate(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, ErrNoIdentity.Error(), http.StatusUnauthorized)
	})
}
func (noIdentity) Protect(next http.Handler) http.Handler  { return next }
func (noIdentity) Entitled(context.Context, Session) error { return nil }
func (noIdentity) Forward(*http.Request) map[string]string { return nil }
func (noIdentity) Routes(*Routes)                          {}

// ident is the installed Identity, or the one that refuses everyone.
func (s *Server) ident() Identity {
	if s.identity == nil {
		return noIdentity{}
	}
	return s.identity
}
