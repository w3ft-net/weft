//go:build !defaultshape

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

type fixedScoper struct{ set shape.Set }

func (f fixedScoper) ReadScope(context.Context, tenancy.Actor) (shape.Set, error) { return f.set, nil }

// A source is a coordinate, so the source endpoints are filtered by the same
// scope as a query, not by namespace name: a scope that reads only alpha's api
// service shows alpha/api and nothing of alpha/web.
func TestSourceEndpoints_FilteredByCoordinates(t *testing.T) {
	srv, s := testServer(t)
	name := fmt.Sprintf("srccoord-%d", os.Getpid())
	cust, _ := s.FindOrCreateOrg(name)
	member, _ := s.CreateVerifiedUser(name + "@example.com")
	if err := s.AddTeamMemberWithEmail(cust.ID, member.ID, member.Email, orgs.RoleMember); err != nil {
		t.Fatal(err)
	}
	seedPendingSource(t, srv, cust.ID, "alpha", "api")
	seedPendingSource(t, srv, cust.ID, "alpha", "web")
	seedPendingSource(t, srv, cust.ID, "beta", "api")
	srv.tenancy.Scope = fixedScoper{shape.Set{{Customer: int64(cust.ID), Namespace: shape.Value("alpha"), Service: shape.Value("api")}}}
	handler := srv.Handler()
	jwt, _ := auth.Generate(member, "test-secret")

	get := func(path string) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	services := func(body string) []string {
		var rows []struct{ Namespace, Service string }
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Namespace+"/"+r.Service)
		}
		return out
	}

	for _, path := range []string{"/api/sources/pending", "/api/sources"} {
		code, body := get(path)
		if got := strings.Join(services(body), ","); code != 200 || got != "alpha/api" {
			t.Errorf("%s = %d %v, want only alpha/api", path, code, got)
		}
	}
	code, body := get("/api/sources/pending/count")
	var cnt struct{ Count int }
	json.Unmarshal([]byte(body), &cnt)
	if code != 200 || cnt.Count != 1 {
		t.Errorf("pending/count = %d %s, want 1", code, body)
	}
	for _, p := range []string{"/api/sources/alpha/web/schema", "/api/sources/beta/api/schema"} {
		if code, _ := get(p); code != 404 {
			t.Errorf("%s = %d, want 404, the same as a source that doesn't exist", p, code)
		}
	}
	if code, _ := get("/api/sources/alpha/api/schema"); code != 200 {
		t.Errorf("schema of the readable source = %d, want 200", code)
	}
}
