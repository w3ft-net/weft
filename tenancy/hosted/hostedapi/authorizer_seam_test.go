package hostedapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
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

// productWith is a product over a server that has no store, whose Authorizer is az.
func productWith(az tenancy.Authorizer) *product {
	srv := weftapi.NewServer(nil, "", "http://localhost")
	srv.SetTenancy(tenancy.Adapter{Authz: az})
	p := newProduct()
	p.attach(srv)
	return p
}

// The product's action gates ask the Authorizer too (see api's
// TestActionGates_AskTheAuthorizer). An org owner would pass today's IsOwner
// checks, so a refusal here can only come from the injected Authorizer.
func TestActionGates_AskTheAuthorizer(t *testing.T) {
	srv, s := testServer(t)
	srv.SetTenancy(tenancy.Adapter{Authz: denyAll{}, Scope: nil, ActorOf: nil})
	handler := srv.Handler()

	name := fmt.Sprintf("hostedapi-authzseam-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	s.FindOrCreateNamespace(cust.ID, "x")
	jwt, _ := auth.Generate(user, "test-secret")

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/billing/setup-intent", `{}`},
		{"POST", "/api/billing/checkout", `{}`},
		{"GET", "/api/admin/customers", ``},
		{"POST", "/api/team/invite", `{"email":"a@b.test"}`},
		{"PUT", "/api/permissions", `{}`},
		{"PUT", "/api/namespaces/x/permissions/someone", `{}`},
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
