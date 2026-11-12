//go:build !defaultshape

package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// The source endpoints expose coordinates and real log lines (a pending
// source's sample), so they are held to the same per-namespace read permission
// as /log: a member who can read only alpha sees nothing of beta's sources.
func TestSourceEndpoints_FilteredByNamespaceRead(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("srcscope-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	member, _ := s.CreateVerifiedUser(name + "@example.com")
	if err := s.AddTeamMemberWithEmail(cust.ID, member.ID, member.Email, orgs.RoleMember); err != nil {
		t.Fatal(err)
	}
	seedPendingSource(t, srv, cust.ID, "alpha", "api")
	seedPendingSource(t, srv, cust.ID, "beta", "web")
	alpha, _ := s.FindOrCreateNamespace(cust.ID, "alpha")
	if err := s.UpsertNamespacePermission(&store.NamespacePermission{UserID: member.ID, NamespaceID: alpha.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	jwt, _ := auth.Generate(member, "test-secret")

	get := func(path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := get("/api/sources/pending")
	if code != 200 {
		t.Fatalf("pending = %d: %s", code, body)
	}
	if strings.Contains(body, "beta") || !strings.Contains(body, "alpha") {
		t.Errorf("pending = %s, want alpha only (beta's sample lines must not leak)", body)
	}

	code, body = get("/api/sources/pending/count")
	var cnt struct{ Count int }
	json.Unmarshal([]byte(body), &cnt)
	if code != 200 || cnt.Count != 1 {
		t.Errorf("pending/count = %d %s, want 1", code, body)
	}

	code, body = get("/api/sources")
	if code != 200 || strings.Contains(body, "beta") {
		t.Errorf("list = %d %s, want no beta", code, body)
	}

	code, body = get("/api/sources/beta/web/schema")
	if code != 404 || strings.Contains(body, "line one") {
		t.Errorf("schema of unreadable source = %d %s, want 404 with no sample", code, body)
	}
	if code, _ = get("/api/sources/alpha/api/schema"); code != 200 {
		t.Errorf("schema of readable source = %d, want 200", code)
	}
}
