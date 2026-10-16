package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
)

// TestGraduateOnboarding_RevokesOnlyLoom pins graduation's contract: the
// tour ends by revoking the user's Loom demo seat, and nothing else. The
// user keeps their own org and lands back in it. graduateOnboarding used
// to update memberships with only `user_id = ?` — the org context did not
// scope that query — so it deactivated every membership the user held,
// including their own org. The next /api/auth/me then 403'd and the
// client sent a freshly graduated comp user to the login page, locked
// out of their account.
func TestGraduateOnboarding_RevokesOnlyLoom(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	loomCust, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom customer: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loomCust.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	ownerEmail := fmt.Sprintf("grad-owner-%d@acme.example.com", os.Getpid())
	body, _ := json.Marshal(map[string]string{
		"email": ownerEmail, "org_name": "GradCo", "tier": "bolt", "reason": "design-partner",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	owner, err := s.GetUserByEmail(ownerEmail)
	if err != nil || owner == nil {
		t.Fatalf("owner user not created: %v", err)
	}

	if err := srv.graduateOnboarding(context.Background(), owner.ID); err != nil {
		t.Fatalf("graduateOnboarding: %v", err)
	}

	orgsAfter, err := s.ListUserOrgs(owner.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	var inLoom, inOwn bool
	for _, o := range orgsAfter {
		switch o.Name {
		case LoomCustomerSlug:
			inLoom = true
		case "gradco":
			inOwn = true
		}
	}
	if inLoom {
		t.Errorf("Loom membership survived graduation; orgs=%+v", orgsAfter)
	}
	if !inOwn {
		t.Fatalf("graduation revoked the user's OWN org membership (they would be locked out); orgs=%+v", orgsAfter)
	}
}
