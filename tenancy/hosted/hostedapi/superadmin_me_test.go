package hostedapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// TestMe_SuperadminWithoutMembership pins the real-world logout bug: the
// production superadmin (paul@graybeardcorp.com) belongs to no customer
// org. BuildSession returned ErrNoMembership for such a user, so
// AuthMiddleware 403'd every authed request — including GET /api/auth/me.
// The SPA treats any non-2xx /me as logged-out (it wipes the token in
// AuthContext's catch), so the first hard navigation after login (e.g.
// typing /admin) booted the operator straight back to the login screen.
//
// A superadmin is a platform operator who may legitimately have zero org
// memberships; /me must still come back 200 so the session survives.
func TestMe_SuperadminWithoutMembership(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	// Superadmin with NO team membership and no last_active_org_id —
	// exactly the production operator's shape.
	email := fmt.Sprintf("super-nomem-%d@example.com", os.Getpid())
	user, err := s.CreateVerifiedUser(email)
	if err != nil {
		t.Fatalf("create superadmin: %v", err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).
		Update("is_superadmin", true).Error; err != nil {
		t.Fatalf("promote superadmin: %v", err)
	}
	user.IsSuperadmin = true

	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("me for membership-less superadmin: expected 200, got %d: %s\n(this is the bug — a non-200 here makes the SPA wipe the token and show the login screen)",
			w.Code, w.Body.String())
	}

	var me map[string]any
	json.Unmarshal(w.Body.Bytes(), &me)
	if su, _ := me["is_superadmin"].(bool); !su {
		t.Error("expected is_superadmin=true in /me for the operator")
	}
	// No org to scope to — customer is empty and the org list is bare.
	if cust, _ := me["customer"].(string); cust != "" {
		t.Errorf("expected empty active customer for org-less superadmin, got %q", cust)
	}
}
