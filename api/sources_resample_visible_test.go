package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// A configured weftd source being resampled keeps ingesting, so its row stays
// "configured". The user still has to be able to see it: the pending list
// carries it (sampling while collecting, pending once the fresh sample is
// ready) and the source list reports the resample's progress.
func TestResampleOfConfiguredSourceIsVisible(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	name := fmt.Sprintf("resamplevis-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	user, _ := s.CreateVerifiedUser(name + "@example.com")
	dashTeamMember(s, cust, user, name)
	jwt, _ := auth.Generate(user, "test-secret")
	ns, _ := s.FindOrCreateNamespace(cust.ID, "ns1")

	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", `["a"]`, `{}`, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureLogGroup(context.Background(), ns.ID, "api", "app: api\nformat: text\n"); err != nil {
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
	type pending struct {
		Service         string `json:"service"`
		Status          string `json:"status"`
		SampleCollected int    `json:"sample_collected"`
	}
	type listed struct {
		Service  string `json:"service"`
		Status   string `json:"status"`
		Resample *struct {
			Collected int  `json:"collected"`
			Target    int  `json:"target"`
			Ready     bool `json:"ready"`
		} `json:"resample"`
	}
	find := func(rows []listed) *listed {
		for i := range rows {
			if rows[i].Service == "api" {
				return &rows[i]
			}
		}
		return nil
	}

	var p []pending
	var l []listed
	get("/api/sources/pending", &p)
	get("/api/sources", &l)
	if len(p) != 0 || find(l) == nil || find(l).Resample != nil {
		t.Fatalf("before resample: pending=%v listed=%+v, want nothing pending and no resample", p, find(l))
	}

	if err := s.RequestResample(ns.ID, "api", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", `["f1","f2"]`, `{}`, 2, true); err != nil {
		t.Fatal(err)
	}
	p, l = nil, nil
	get("/api/sources/pending", &p)
	get("/api/sources", &l)
	if len(p) != 1 || p[0].Service != "api" || p[0].Status != "sampling" || p[0].SampleCollected != 2 {
		t.Errorf("mid-resample pending = %+v, want api sampling 2", p)
	}
	row := find(l)
	if row == nil || row.Status != "configured" || row.Resample == nil ||
		row.Resample.Collected != 2 || row.Resample.Target != store.SampleTarget || row.Resample.Ready {
		t.Errorf("mid-resample list row = %+v (resample %+v), want configured + resample 2/%d not ready", row, row.Resample, store.SampleTarget)
	}

	if err := s.UpsertPendingLogGroup(ns.ID, "api", "i", "d", `["f1","f2","f3"]`, `{}`, 3, false); err != nil {
		t.Fatal(err)
	}
	p, l = nil, nil
	get("/api/sources/pending", &p)
	get("/api/sources", &l)
	if len(p) != 1 || p[0].Status != "pending" {
		t.Errorf("finished resample pending = %+v, want api pending", p)
	}
	if row := find(l); row == nil || row.Resample == nil || !row.Resample.Ready {
		t.Errorf("finished resample list row = %+v, want resample.ready", row)
	}
}
