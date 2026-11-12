//go:build !defaultshape

package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
)

// DELETE /api/namespaces/{name}: owner only; an empty namespace goes; one with
// sources or live tokens is refused with a 409 that names them, unless
// ?force=true, which removes it all.
func TestRemoveNamespace(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("nsrm-%d", os.Getpid()))
	_, ownerJWT := permsSeed(t, s, cust, store.RoleOwner, "nsrm-owner")
	_, memberJWT := permsSeed(t, s, cust, orgs.RoleMember, "nsrm-member")

	del := func(jwt, path string) (int, string) {
		req := httptest.NewRequest("DELETE", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	empty, _ := s.FindOrCreateNamespace(cust.ID, "empty")
	if code, _ := del(memberJWT, "/api/namespaces/empty"); code != 403 {
		t.Errorf("member delete = %d, want 403", code)
	}
	if _, err := s.GetNamespace(cust.ID, "empty"); err != nil {
		t.Fatalf("a refused delete removed the namespace: %v", err)
	}
	if code, body := del(ownerJWT, "/api/namespaces/empty"); code != 204 {
		t.Fatalf("owner delete of empty namespace = %d: %s", code, body)
	}
	if _, err := s.GetNamespace(cust.ID, "empty"); err == nil {
		t.Errorf("namespace %d still exists after delete", empty.ID)
	}
	if code, _ := del(ownerJWT, "/api/namespaces/empty"); code != 404 {
		t.Errorf("deleting a missing namespace = %d, want 404", code)
	}

	full, _ := s.FindOrCreateNamespace(cust.ID, "full")
	if err := s.UpsertPendingLogGroup(full.ID, "api", "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAgentToken(cust.ID, full.ID, "t", ""); err != nil {
		t.Fatal(err)
	}
	code, body := del(ownerJWT, "/api/namespaces/full")
	if code != 409 {
		t.Fatalf("delete of non-empty namespace = %d, want 409: %s", code, body)
	}
	var refusal struct {
		Sources []string `json:"sources"`
		Tokens  int      `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || len(refusal.Sources) != 1 || refusal.Sources[0] != "api" || refusal.Tokens != 1 {
		t.Errorf("409 body = %q (%v), want sources [api] and 1 token", body, err)
	}
	if _, err := s.GetNamespace(cust.ID, "full"); err != nil {
		t.Fatalf("a refused delete removed the namespace: %v", err)
	}

	if code, body := del(ownerJWT, "/api/namespaces/full?force=true"); code != 204 {
		t.Fatalf("forced delete = %d: %s", code, body)
	}
	if _, err := s.GetNamespace(cust.ID, "full"); err == nil {
		t.Error("forced delete left the namespace behind")
	}
}
