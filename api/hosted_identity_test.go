//go:build !defaultshape

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// testSecret signs the JWTs the api tests mint with auth.Generate.
const testSecret = "test-secret"

// jwtIdentity is the hosted identity as the core tests see it: bearer JWTs,
// sessions in the org the X-Active-Org header selects, a verified-email gate on
// protected routes and a subscription gate on the shell. It registers no routes;
// the sign-in and account routes are hosted's and are tested in hostedapi. It
// lives here, and not in a package the core imports, so the core still names no
// hosted package.
type jwtIdentity struct{ st *store.Store }

var _ Identity = jwtIdentity{}

func (i jwtIdentity) Authenticate(next http.Handler) http.Handler {
	return auth.Middleware(i.st, testSecret, RecordAccessIdentity)(next)
}

func (jwtIdentity) Protect(next http.Handler) http.Handler { return auth.RequireEmailVerified(next) }

func (i jwtIdentity) Entitled(_ context.Context, s Session) error {
	if sub, _ := i.st.ActiveSubscription(s.Org()); sub == nil {
		return errors.New("no active subscription")
	}
	return nil
}

func (jwtIdentity) Forward(r *http.Request) map[string]string {
	out := map[string]string{}
	if a := r.Header.Get("Authorization"); a != "" {
		out["authorization"] = a
	}
	if org := r.Header.Get(auth.ActiveOrgHeader); org != "" {
		out["x-active-org"] = org
	}
	return out
}

func (jwtIdentity) ShellRan(context.Context, Session, ShellRun) map[string]any { return nil }
func (jwtIdentity) Routes(*Routes)                                             {}
