package hostedapi

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// Granting a permission on a namespace that doesn't exist is a 404, not a
// silent creation. The handler already answered "namespace not found", but
// got there through FindOrCreateNamespace, which creates it.
func TestSetNamespacePermission_UnknownNamespaceIsNotCreated(t *testing.T) {
	srv, s := testServer(t)
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("ns-perm-missing-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.CreateVerifiedUser(fmt.Sprintf("ns-perm-missing-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("PUT", "/api/namespaces/typo/permissions/some-user", strings.NewReader(`{"can_read":true}`))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if _, err := s.GetNamespace(cust.ID, "typo"); err == nil {
		t.Errorf("the permission call created namespace typo")
	}
}
