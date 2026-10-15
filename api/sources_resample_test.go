package api

import (
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// A pending source can be sent back to sampling over HTTP, and a source that
// is still on its first sample answers 409 instead of silently succeeding.
func TestResampleSource_Pending(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("resamp-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")
	seedPendingSource(t, srv, cust.ID, "ns1", "worker")

	post := func(path string) int {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	if code := post("/api/sources/ns1/worker/resample"); code != 200 {
		t.Fatalf("resample pending = %d, want 200", code)
	}
	lg, _ := s.FindLogGroup(cust.ID, "ns1", "worker")
	if lg.Status != store.StatusSampling || lg.Sample != "" || lg.ResampleRequestedAt == nil {
		t.Errorf("status=%q sample=%q flag=%v, want sampling, cleared, flagged", lg.Status, lg.Sample, lg.ResampleRequestedAt)
	}
	if code := post("/api/sources/ns1/worker/resample"); code != 409 {
		t.Errorf("resample while sampling = %d, want 409", code)
	}
	if code := post("/api/sources/ns1/nope/resample"); code != 404 {
		t.Errorf("resample unknown source = %d, want 404", code)
	}
}
