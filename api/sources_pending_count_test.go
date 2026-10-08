package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
)

// The sidebar polls every 30s per open tab and only needs a number. It used to
// fetch the full pending list, including each source's sample and detection
// blobs, just to take its length.
func TestPendingSourcesCount(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("pendcount-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	other, _ := s.FindOrCreateOrg(name + "-other")
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")

	count := func() int {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/sources/pending/count", nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("pending/count = %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return resp.Count
	}

	if n := count(); n != 0 {
		t.Fatalf("no sources yet: count = %d, want 0", n)
	}

	seedPendingSource(t, srv, cust.ID, "ns1", "worker")
	seedPendingSource(t, srv, cust.ID, "ns2", "api")
	// Still accumulating its sample: counts as pending, like the list does.
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns3")
	if err := s.UpsertPendingLogGroup(ns.ID, "cron", "i-test", "d", `["a"]`, `{}`, 1, true); err != nil {
		t.Fatal(err)
	}
	// Another org's pending source must not leak into this count.
	seedPendingSource(t, srv, other.ID, "ns1", "worker")

	if n := count(); n != 3 {
		t.Fatalf("count = %d, want 3 (two pending + one sampling in this org, none from the other)", n)
	}
}
