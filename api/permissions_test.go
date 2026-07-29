package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"

	"github.com/graybeardcorp/saaskit/orgs"
)

// permsSeed mirrors dashTeamMember: it just adds (user, role) as a
// team_members row so BuildSession resolves an active org. Returns
// the user's signed JWT.
func permsSeed(t *testing.T, s *store.Store, cust *store.Organization, role, emailSuffix string) (*store.User, string) {
	t.Helper()
	user, err := s.CreateVerifiedUser(fmt.Sprintf("%s-%d@example.com", emailSuffix, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	err = s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, orgs.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	jwt, _ := auth.Generate(user, "test-secret")
	return user, jwt
}

func TestPermissions_GetAndPut(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("perms-api-%d", os.Getpid()))
	_, ownerJWT := permsSeed(t, s, cust, "owner", "owner")
	member, _ := permsSeed(t, s, cust, "member", "member")
	_, _ = s.FindOrCreateNamespace(cust.ID, "api")

	// PUT: owner grants member edit on /bin + read on /log/api.
	put := store.TeamPermsDoc{
		member.Email: {
			Bin: store.AccessEdit, Cron: store.AccessReadOnly, Etc: store.AccessReadOnly,
			Namespaces: map[string]store.NamespaceAcl{
				"api": {Read: true},
			},
		},
	}
	body, _ := json.Marshal(put)
	req := httptest.NewRequest("PUT", "/api/permissions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerJWT)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	// GET: should round-trip.
	req = httptest.NewRequest("GET", "/api/permissions", nil)
	req.Header.Set("Authorization", "Bearer "+ownerJWT)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	var got store.TeamPermsDoc
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	if got[member.Email].Bin != store.AccessEdit {
		t.Errorf("expected member.Bin == edit, got %+v", got)
	}
	if !got[member.Email].Namespaces["api"].Read {
		t.Errorf("expected api.Read == true, got %+v", got)
	}
}

func TestPermissions_Put_NonOwnerForbidden(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("perms-noown-%d", os.Getpid()))
	_, memberJWT := permsSeed(t, s, cust, "member", "lurker")

	req := httptest.NewRequest("PUT", "/api/permissions", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+memberJWT)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPermissions_Put_RejectsUnknownMember(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("perms-badmem-%d", os.Getpid()))
	_, ownerJWT := permsSeed(t, s, cust, "owner", "lonely-owner")

	put := store.TeamPermsDoc{
		"ghost@example.com": {Bin: store.AccessEdit, Cron: store.AccessReadOnly, Etc: store.AccessReadOnly},
	}
	body, _ := json.Marshal(put)
	req := httptest.NewRequest("PUT", "/api/permissions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerJWT)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}
