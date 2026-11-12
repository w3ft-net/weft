package hostedapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
)

// DELETE /api/team/{id} takes a membership id from the URL. It must only ever
// find memberships in the caller's active org: an owner of another org holding
// a leaked id gets the same 404 as a nonexistent one.
func TestRemoveTeamMember_OtherOrgsMemberIs404(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	orgA, _ := s.FindOrCreateOrg(fmt.Sprintf("idor-a-%d", os.Getpid()))
	orgB, _ := s.FindOrCreateOrg(fmt.Sprintf("idor-b-%d", os.Getpid()))
	permsSeed(t, s, orgA, store.RoleOwner, "idor-a-owner")
	victim, _ := permsSeed(t, s, orgA, orgs.RoleMember, "idor-a-victim")
	_, ownerBJWT := permsSeed(t, s, orgB, store.RoleOwner, "idor-b-owner")

	m, err := s.GetTeamMemberByEmail(orgA.ID, victim.Email)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/api/team/"+m.ID, nil)
	req.Header.Set("Authorization", "Bearer "+ownerBJWT)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if got, err := s.GetTeamMemberByEmail(orgA.ID, victim.Email); err != nil {
		t.Errorf("org B's owner removed org A's member: %+v, %v", got, err)
	}
}

func TestRemoveTeamMember_OwnersCannotBeRemoved(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	org, _ := s.FindOrCreateOrg(fmt.Sprintf("idor-own-%d", os.Getpid()))
	_, ownerJWT := permsSeed(t, s, org, store.RoleOwner, "idor-own-owner")
	other, _ := permsSeed(t, s, org, store.RoleOwner, "idor-own-other")

	m, err := s.GetTeamMemberByEmail(org.ID, other.Email)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("DELETE", "/api/team/"+m.ID, nil)
	req.Header.Set("Authorization", "Bearer "+ownerJWT)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403: %s", w.Code, w.Body.String())
	}
	if got, _ := s.GetTeamMemberByEmail(org.ID, other.Email); got == nil {
		t.Error("owner was removed by another owner")
	}
}
