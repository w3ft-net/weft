// Package basic is an identity of one HTTP Basic credential, for a build with one
// user and no sign-in page: the api's half (Identity) and the coordinator's
// (Credential.Server). The credential comes from the environment
// (WEFT_BASIC_AUTH_USER and WEFT_BASIC_AUTH_PASSWORD), there is one principal and
// no sign-in route.
package basic

import (
	"context"
	"encoding/json"
	"net/http"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/identity"
)

// Identity authenticates every request by its HTTP Basic credential.
type Identity struct {
	Credential Credential
}

// FromEnv is the identity of the credential in the environment. It fails with
// ErrNoCredential when there is none, so an open api that could not
// authenticate anyone does not start.
func FromEnv() (*Identity, error) {
	b, err := CredentialFromEnv()
	if err != nil {
		return nil, err
	}
	return &Identity{Credential: b}, nil
}

var _ weftapi.Identity = (*Identity)(nil)

// Authenticate answers 401, asking for the credential, to a request without it.
func (i *Identity) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !i.Credential.Authenticate(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Basic realm="weft", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		weftapi.RecordAccessIdentity(r, i.Credential.User)
		next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), i.Credential.Caller())))
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

// Routes registers /api/auth/me, the one route the web app asks to learn how it
// is signed in. The browser holds no token here: it answers the Basic challenge
// itself and sends the credential on every request, so session is "open" and
// there is no sign-in page.
func (i *Identity) Routes(rt *weftapi.Routes) {
	rt.Authed("GET /api/auth/me", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"id": i.Credential.User, "email": i.Credential.User, "session": "open",
		})
	}))
}
