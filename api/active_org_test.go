package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// A session that acts in no group (a superadmin who belongs to no org) has no
// group data. Handlers must refuse it with 403 rather than resolve the empty
// name to an org, which used to create an org named "" on the fly.
func TestRequireGroup_NoGroupIs403(t *testing.T) {
	shapetest.NeedsGroup(t)
	w := httptest.NewRecorder()
	if groupServer().requireGroup(w, stubSession{}) {
		t.Fatal("requireGroup(no group) = true, want false")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestRequireGroup_AllowsASessionInAGroup(t *testing.T) {
	shapetest.NeedsGroup(t)
	w := httptest.NewRecorder()
	if !groupServer().requireGroup(w, stubSession{customer: 42}) {
		t.Fatal("requireGroup(group 42) = false, want true")
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("wrote a response for a valid session: %d %q", w.Code, w.Body.String())
	}
}

// Until the deployment says what a group is, every partition is one.
func TestRequireGroup_DefaultsToEveryPartition(t *testing.T) {
	if !(&Server{}).requireGroup(httptest.NewRecorder(), stubSession{}) {
		t.Fatal("a server with no SetGroups refused a session")
	}
}

// stubSession is a Session in a given group.
type stubSession struct{ customer int64 }

func (s stubSession) Principal() identity.Principal { return stubPrincipal{customer: s.customer} }
func (stubSession) UserID() string                  { return "" }

// stubPrincipal acts in group n of the build's shape.
type stubPrincipal struct{ customer int64 }

func (a stubPrincipal) Partition() shape.Added { return shapetest.Added(a.customer) }
func (stubPrincipal) Subject() string          { return "" }

func groupServer() *Server {
	s := &Server{}
	s.SetGroups(func(a shape.Added) bool { return a != shapetest.Added(0) })
	return s
}
