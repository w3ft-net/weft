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

func TestSharedLinks_ResolveForMembersOnly(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("sharedapi-%d", os.Getpid()))
	member, _ := s.CreateUser(fmt.Sprintf("sl-member-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(cust.ID, member.ID, member.Email, store.RoleOwner)
	memberJWT, _ := auth.Generate(member, "test-secret")

	otherOrg, _ := s.FindOrCreateOrg(fmt.Sprintf("sharedapi-other-%d", os.Getpid()))
	outsider, _ := s.CreateUser(fmt.Sprintf("sl-out-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(otherOrg.ID, outsider.ID, outsider.Email, store.RoleOwner)
	outsiderJWT, _ := auth.Generate(outsider, "test-secret")

	slug, err := s.SaveSharedLink(cust.ID, member.ID, "/bin/scan", "1h..", "--app=loom")
	if err != nil {
		t.Fatal(err)
	}

	get := func(jwt, slug string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/shared-links/"+slug, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	w := get(memberJWT, slug)
	if w.Code != 200 {
		t.Fatalf("member: %d %s", w.Code, w.Body.String())
	}
	var got struct{ Bin, Range, Args string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Bin != "/bin/scan" || got.Range != "1h.." || got.Args != "--app=loom" {
		t.Errorf("body = %s", w.Body.String())
	}
	if w := get(outsiderJWT, slug); w.Code != 404 {
		t.Errorf("another org: %d, want 404", w.Code)
	}
	if w := get(memberJWT, "nosuchslug"); w.Code != 404 {
		t.Errorf("unknown slug: %d, want 404", w.Code)
	}
}
