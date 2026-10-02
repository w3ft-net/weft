package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// A session with no active org (a superadmin who belongs to none) has no
// org-scoped data. Handlers must refuse it with 403 rather than resolve the
// empty name to an org — which used to create an org named "" on the fly.
func TestActiveOrg_NoActiveOrgIs403(t *testing.T) {
	w := httptest.NewRecorder()
	id, ok := activeOrg(w, &auth.Session{})
	if ok || id != 0 {
		t.Fatalf("activeOrg(no org) = (%d, %v), want (0, false)", id, ok)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestActiveOrg_ReturnsTheSessionsOrg(t *testing.T) {
	w := httptest.NewRecorder()
	id, ok := activeOrg(w, &auth.Session{ActiveOrgID: store.OrgID(42)})
	if !ok || id != 42 {
		t.Fatalf("activeOrg = (%d, %v), want (42, true)", id, ok)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("wrote a response for a valid session: %d %q", w.Code, w.Body.String())
	}
}
