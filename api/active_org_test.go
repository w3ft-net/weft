package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/tenancy"
)

// A session that acts in no tenant (a superadmin who belongs to no org) has no
// tenant data. Handlers must refuse it with 403 rather than resolve the empty
// name to an org, which used to create an org named "" on the fly.
func TestRequireTenant_NoTenantIs403(t *testing.T) {
	shapetest.NeedsTenant(t)
	w := httptest.NewRecorder()
	if tenantServer().requireTenant(w, stubSession{}) {
		t.Fatal("requireTenant(no tenant) = true, want false")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestRequireTenant_AllowsASessionInATenant(t *testing.T) {
	shapetest.NeedsTenant(t)
	w := httptest.NewRecorder()
	if !tenantServer().requireTenant(w, stubSession{customer: 42}) {
		t.Fatal("requireTenant(tenant 42) = false, want true")
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("wrote a response for a valid session: %d %q", w.Code, w.Body.String())
	}
}

// Until the deployment says what a tenant is, every partition is one.
func TestRequireTenant_DefaultsToEveryPartition(t *testing.T) {
	if !(&Server{}).requireTenant(httptest.NewRecorder(), stubSession{}) {
		t.Fatal("a server with no SetTenants refused a session")
	}
}

// stubSession is a Session in a given tenant.
type stubSession struct{ customer int64 }

func (s stubSession) Actor() tenancy.Actor { return stubActor{customer: s.customer} }
func (stubSession) UserID() string         { return "" }

// stubActor acts in tenant n of the build's shape.
type stubActor struct{ customer int64 }

func (a stubActor) Partition() shape.Added { return shapetest.Added(a.customer) }
func (stubActor) Subject() string          { return "" }

func tenantServer() *Server {
	s := &Server{}
	s.SetTenants(func(a shape.Added) bool { return a != shapetest.Added(0) })
	return s
}
