package api

import (
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
)

// TestAuthMiddlewareDoesNotRewriteUnchangedLastActiveOrg pins that an
// authenticated request in the org the user is already pinned to writes
// nothing to users. Every request used to UPDATE users (bumping updated_at),
// which is a dead tuple plus WAL per request for no change.
func TestAuthMiddlewareDoesNotRewriteUnchangedLastActiveOrg(t *testing.T) {
	srv, s := testServer(t)
	h := srv.Handler()

	name := fmt.Sprintf("lastactive-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	call := func() {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		if rec := httptest.NewRecorder(); true {
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("/api/auth/me = %d: %s", rec.Code, rec.Body.String())
			}
		}
	}
	// ctid changes whenever the row is rewritten (an UPDATE writes a new tuple
	// version), even within the test's single transaction.
	ctid := func() string {
		t.Helper()
		var c string
		if err := s.DB().Raw(`SELECT ctid::text FROM users WHERE id = ?`, user.ID).Scan(&c).Error; err != nil {
			t.Fatal(err)
		}
		return c
	}

	call() // may legitimately pin last_active_org_id on first use
	settled := ctid()
	call()
	call()
	if got := ctid(); got != settled {
		t.Fatalf("users row rewritten by repeat requests in the same org: ctid %s -> %s", settled, got)
	}
}

// UpdateUserLastActiveOrg documents itself as a no-op when nothing changed.
func TestUpdateUserLastActiveOrgIsNoopWhenUnchanged(t *testing.T) {
	_, s := testServer(t)
	name := fmt.Sprintf("lastactive-store-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateUser(name + "@example.com")

	if err := s.UpdateUserLastActiveOrg(user.ID, cust.ID); err != nil {
		t.Fatal(err)
	}
	var before string
	s.DB().Raw(`SELECT ctid::text FROM users WHERE id = ?`, user.ID).Scan(&before)
	if err := s.UpdateUserLastActiveOrg(user.ID, cust.ID); err != nil {
		t.Fatal(err)
	}
	var after string
	s.DB().Raw(`SELECT ctid::text FROM users WHERE id = ?`, user.ID).Scan(&after)
	if before != after {
		t.Fatalf("same-org update rewrote the row: ctid %s -> %s", before, after)
	}
}
