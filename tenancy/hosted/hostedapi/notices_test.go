package hostedapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/notices"
	"github.com/graybeardcorp/weft/store"
)

func listNotices(t *testing.T, srv *product, jwt string) []noticeResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/notices", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /api/notices: status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Notices []noticeResponse `json:"notices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode notices: %v (body %s)", err, rec.Body.String())
	}
	return body.Notices
}

func markSeen(t *testing.T, srv *product, jwt string, id int64) int {
	t.Helper()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/notices/%d/seen", id), nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestNoticesAPI_ListAndDismiss(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notices-api-%d", os.Getpid()))
	user, jwt := permsSeed(t, s, cust, orgs.RoleMember, "notices")

	err := s.EmitNotice(user.ID, notices.KeyMemberRetainedGrants, "warning",
		map[string]any{"member": "dana@x.test", "groups": []string{"binmgr"}})
	if err != nil {
		t.Fatal(err)
	}

	got := listNotices(t, srv, jwt)
	if len(got) != 1 {
		t.Fatalf("want 1 notice, got %d: %+v", len(got), got)
	}
	if got[0].Key != notices.KeyMemberRetainedGrants {
		t.Errorf("key = %q", got[0].Key)
	}
	if got[0].Title == "" || got[0].Body == "" {
		t.Errorf("notice should arrive rendered; got %+v", got[0])
	}

	// Listing again still returns it: reading is not delivering.
	if again := listNotices(t, srv, jwt); len(again) != 1 {
		t.Errorf("second list: want 1, got %d", len(again))
	}

	if code := markSeen(t, srv, jwt, got[0].ID); code != 204 {
		t.Fatalf("dismiss: status %d, want 204", code)
	}
	if after := listNotices(t, srv, jwt); len(after) != 0 {
		t.Errorf("after dismiss: want 0, got %d", len(after))
	}
}

func TestNoticesAPI_CannotDismissAnotherUsersNotice(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notices-api-x-%d", os.Getpid()))
	alice, _ := permsSeed(t, s, cust, orgs.RoleMember, "notices-alice")
	_, bobJWT := permsSeed(t, s, cust, orgs.RoleMember, "notices-bob")

	if err := s.EmitNotice(alice.ID, "tmp_48h_expiry", "warning", nil); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingNotices(alice.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("setup: pending=%d err=%v", len(pending), err)
	}

	if code := markSeen(t, srv, bobJWT, pending[0].ID); code != 404 {
		t.Errorf("bob dismissing alice's notice: status %d, want 404", code)
	}
	after, _ := s.PendingNotices(alice.ID)
	if len(after) != 1 {
		t.Errorf("alice's notice should survive bob's attempt; pending=%d", len(after))
	}
}

func TestNoticesAPI_UnknownKeyNotListed(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notices-api-d-%d", os.Getpid()))
	user, jwt := permsSeed(t, s, cust, orgs.RoleMember, "notices-drift")

	if err := s.EmitNotice(user.ID, "from_a_newer_build", "warning", nil); err != nil {
		t.Fatal(err)
	}
	if got := listNotices(t, srv, jwt); len(got) != 0 {
		t.Errorf("unrenderable notice listed: %+v", got)
	}
	pending, _ := s.PendingNotices(user.ID)
	if len(pending) != 1 {
		t.Errorf("unrenderable notice should stay pending; got %d", len(pending))
	}
}

func TestNoticesAPI_BadID(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notices-api-b-%d", os.Getpid()))
	_, jwt := permsSeed(t, s, cust, orgs.RoleMember, "notices-bad")

	req := httptest.NewRequest("POST", "/api/notices/abc/seen", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("non-numeric id: status %d, want 400", rec.Code)
	}
}

var _ = func(p store.PendingNotice) int64 { return p.ID }

// listNotices calls GET /api/notices as the given user.

// markSeen calls POST /api/notices/{id}/seen and returns the status.

// TestNoticesAPI_ListAndDismiss: the browser's delivery loop. Listing

// does not mark — the SPA may navigate away before rendering — so the

// notice survives until it is explicitly dismissed.

// TestNoticesAPI_CannotDismissAnotherUsersNotice: ids are global, so the

// dismiss endpoint has to scope to the caller. Otherwise anyone could

// burn anyone's notices by counting up from 1.

// TestNoticesAPI_UnknownKeyNotListed: a row this build can't render is

// withheld rather than shipped blank, and stays pending so a later

// deploy can deliver it.

// TestNoticesAPI_BadID: a non-numeric id is a client error, not a 500.

// TestGraduationNotice_DeliveredInShellClearsBanner: the graduation

// notice is one row with two renderers. Draining it from the shell —

// which takes every pending row — must clear the browser banner, not

// leave it showing something already delivered.

// the dismiss endpoint parses.

// compile-time guard: the api and store packages agree on the id type
