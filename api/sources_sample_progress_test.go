//go:build !defaultshape

package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// A still-filling sample must be watchable: both the pending list and the
// per-source schema endpoint carry the lines seen so far and when the newest
// arrived, so the UI/CLI can render a live progress view.
func TestSamplingProgressExposesPreviewAndLastLine(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("sampprog-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")

	before := time.Now().Add(-time.Second)
	if err := s.UpsertPendingLogGroup(ns.ID, "cron", "i-test", "d", `["line one","line two"]`, `{}`, 2, true); err != nil {
		t.Fatal(err)
	}

	get := func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	type view struct {
		Status          string   `json:"status"`
		SampleCollected int      `json:"sample_collected"`
		Sample          []string `json:"sample"`
		SampleUpdatedAt string   `json:"sample_updated_at"`
	}
	check := func(where string, v view) {
		t.Helper()
		if v.Status != "sampling" || v.SampleCollected != 2 {
			t.Errorf("%s: status=%q collected=%d, want sampling/2", where, v.Status, v.SampleCollected)
		}
		if len(v.Sample) != 2 || v.Sample[1] != "line two" {
			t.Errorf("%s: sample = %v, want the two lines seen so far", where, v.Sample)
		}
		at, err := time.Parse(time.RFC3339, v.SampleUpdatedAt)
		if err != nil || at.Before(before.Truncate(time.Second)) {
			t.Errorf("%s: sample_updated_at = %q (%v), want a fresh RFC3339 time", where, v.SampleUpdatedAt, err)
		}
	}

	var list []view
	get("/api/sources/pending", &list)
	if len(list) != 1 {
		t.Fatalf("pending list = %d rows, want 1", len(list))
	}
	check("pending list", list[0])

	var one view
	get("/api/sources/ns1/cron/schema", &one)
	check("schema endpoint", one)
}
