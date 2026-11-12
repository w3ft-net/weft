package hostedapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// At the final tour step the user leaves Loom by choosing another org in
// the selector. The server does the graduation: the first /me scoped to a
// non-Loom org removes Loom and ends the tour, and that org stays active.
func TestMe_SelectingAnotherOrgAtGraduateLeavesLoom(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	loom, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindOrCreateNamespace(loom.ID, LoomDemoNamespace); err != nil {
		t.Fatal(err)
	}
	// The home org is created first, then a second org the user chooses,
	// so "land in the oldest org" would be the wrong answer.
	home, err := s.FindOrCreateOrg(fmt.Sprintf("grad-home-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := s.FindOrCreateOrg(fmt.Sprintf("grad-chosen-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("grad-switch-%d@example.com", os.Getpid())
	user, err := s.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []store.OrgID{home.ID, chosen.ID} {
		if err := s.AddTeamMemberWithEmail(o, user.ID, email, store.RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.grantLoomMembership(context.Background(), user.ID, email); err != nil {
		t.Fatal(err)
	}
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).
		Update("onboarding_step", "graduate").Error; err != nil {
		t.Fatal(err)
	}

	me := func(org string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("X-Active-Org", org)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("/me as %s: %d %s", org, w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}

	if got := me(LoomCustomerSlug); got["onboarding_step"] != "graduate" {
		t.Fatalf("staying in Loom ended the tour: step=%v", got["onboarding_step"])
	}

	got := me(chosen.Name)
	if got["onboarding_step"] != "done" {
		t.Errorf("step after leaving Loom = %v, want done", got["onboarding_step"])
	}
	if got["customer"] != chosen.Name {
		t.Errorf("active org = %v, want %s", got["customer"], chosen.Name)
	}
	orgs, _ := s.ListUserOrgs(user.ID)
	for _, o := range orgs {
		if o.Name == LoomCustomerSlug {
			t.Errorf("Loom membership survived: %+v", orgs)
		}
	}
	// The chosen org, not the oldest one, is where the next bare request lands.
	if u, _ := s.GetUserByID(user.ID); u.LastActiveOrgID == nil || *u.LastActiveOrgID != chosen.ID {
		t.Errorf("last active org = %v, want %v", u.LastActiveOrgID, chosen.ID)
	}
}
