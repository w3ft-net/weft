package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

type denyAll struct{}

func (denyAll) Authorize(context.Context, tenancy.Actor, tenancy.Action, tenancy.Resource) error {
	return tenancy.Denied("denied by test authorizer")
}

// allowUsers allows every action to the listed users and none to anyone else.
type allowUsers map[string]bool

func (a allowUsers) Authorize(_ context.Context, act tenancy.Actor, _ tenancy.Action, _ tenancy.Resource) error {
	if u, ok := act.(hosted.Actor); ok && a[u.User] {
		return nil
	}
	return tenancy.Denied("denied by test authorizer")
}

// Every action gate asks the Authorizer. An org owner would pass today's
// IsOwner checks, so a refusal here can only come from the injected Authorizer.
func TestActionGates_AskTheAuthorizer(t *testing.T) {
	srv, s := testServer(t)
	srv.authz = denyAll{}
	handler := srv.Handler()

	name := fmt.Sprintf("authzseam-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	s.FindOrCreateNamespace(cust.ID, "x")
	jwt, _ := auth.Generate(user, "test-secret")

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/billing/setup-intent", `{}`},
		{"POST", "/api/billing/checkout", `{}`},
		{"POST", "/api/namespaces", `{"name":"x"}`},
		{"DELETE", "/api/namespaces/x", ``},
		{"POST", "/api/tokens", `{"namespace":"x","label":"l"}`},
		{"POST", "/api/team/invite", `{"email":"a@b.test"}`},
		{"PUT", "/api/permissions", `{}`},
		{"PUT", "/api/namespaces/x/permissions/someone", `{}`},
		{"GET", "/api/admin/customers", ``},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s %s = %d %s, want 403 from the Authorizer", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
