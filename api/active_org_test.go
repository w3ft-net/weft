package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/single"
)

// A session with no active org (a superadmin who belongs to none) has no
// org-scoped data. Handlers must refuse it with 403 rather than resolve the
// empty name to an org — which used to create an org named "" on the fly.
func TestActiveOrg_NoActiveOrgIs403(t *testing.T) {
	w := httptest.NewRecorder()
	id, ok := activeOrg(w, stubSession{})
	if ok || id != 0 {
		t.Fatalf("activeOrg(no org) = (%d, %v), want (0, false)", id, ok)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestActiveOrg_ReturnsTheSessionsOrg(t *testing.T) {
	w := httptest.NewRecorder()
	id, ok := activeOrg(w, stubSession{org: 42})
	if !ok || id != 42 {
		t.Fatalf("activeOrg = (%d, %v), want (42, true)", id, ok)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("wrote a response for a valid session: %d %q", w.Code, w.Body.String())
	}
}

// stubSession is a Session in a given org.
type stubSession struct{ org store.OrgID }

func (s stubSession) Org() store.OrgID   { return s.org }
func (stubSession) Actor() tenancy.Actor { return single.Actor{} }
func (stubSession) UserID() string       { return "" }
