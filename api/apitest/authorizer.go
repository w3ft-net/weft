package apitest

import (
	"context"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/tenancy"
)

type denyAll struct{}

func (denyAll) Authorize(context.Context, tenancy.Actor, tenancy.Action, tenancy.Resource) error {
	return tenancy.Denied("denied by test authorizer")
}

// Authorizer runs the seam between the routes and the build's Authorizer.
func Authorizer(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"ActionGatesAskTheAuthorizer", actionGatesAskTheAuthorizer},
	})
}

// Every action gate asks the Authorizer. The build's owner passes any rule the
// routes once applied themselves, so a refusal here can only come from the
// injected Authorizer.
func actionGatesAskTheAuthorizer(t *testing.T, s Subject) {
	s.namespace(t, "x")
	s.SetAuthz(denyAll{})
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/namespaces", `{"name":"x"}`},
		{"DELETE", "/api/namespaces/x", ``},
		{"POST", "/api/tokens", `{"namespace":"x","label":"l"}`},
	} {
		rec := s.Do(c.method, c.path, []byte(c.body))
		if rec.Code != 403 {
			t.Errorf("%s %s = %d %s, want 403 from the Authorizer", c.method, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}
