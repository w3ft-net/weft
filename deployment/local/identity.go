// The open build's identity for the api: one HTTP Basic credential from the
// environment, one principal, no sign-in routes. It is package api's Identity for a
// deployment that has no users of its own.

package local

import (
	"context"
	"net/http"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/identity"
)

// Identity authenticates every request by its HTTP Basic credential.
type Identity struct {
	Basic defaults.Basic
}

// FromEnv is the identity of the credential in the environment. It fails with
// defaults.ErrNoCredential when there is none, so an open api that could not
// authenticate anyone does not start.
func FromEnv() (*Identity, error) {
	b, err := defaults.BasicFromEnv()
	if err != nil {
		return nil, err
	}
	return &Identity{Basic: b}, nil
}

var _ weftapi.Identity = (*Identity)(nil)

// Authenticate answers 401, asking for the credential, to a request without it.
func (i *Identity) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !i.Basic.Authenticate(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Basic realm="weft", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		weftapi.RecordAccessIdentity(r, i.Basic.User)
		next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), i.Basic.Caller())))
	})
}

// Protect has nothing to add: the one credential is the whole gate.
func (*Identity) Protect(next http.Handler) http.Handler { return next }

// Entitled is always true: the open build has no plan.
func (*Identity) Entitled(context.Context, weftapi.Session) error { return nil }

// Forward carries the credential to the coordinator, whose open identity
// checks it itself.
func (*Identity) Forward(r *http.Request) map[string]string {
	if h := r.Header.Get("Authorization"); h != "" {
		return map[string]string{"authorization": h}
	}
	return nil
}

// ShellRan sends the client nothing.
func (*Identity) ShellRan(context.Context, weftapi.Session, weftapi.ShellRun) map[string]any {
	return nil
}

// Routes registers nothing: there is no sign-in, token or account route in the
// open build.
func (*Identity) Routes(*weftapi.Routes) {}
