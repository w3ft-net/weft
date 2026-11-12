package singleapi_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/tenancy/single"
	"github.com/graybeardcorp/weft/tenancy/single/singleapi"
)

func open() *singleapi.Identity {
	return &singleapi.Identity{Basic: single.Basic{User: "weft", Password: "pw"}}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestFromEnv_RefusesToStartUnconfigured(t *testing.T) {
	t.Setenv(single.BasicUserEnv, "")
	t.Setenv(single.BasicPasswordEnv, "")
	if _, err := singleapi.FromEnv(); err == nil {
		t.Fatal("an open api with no credential started")
	}
}

func TestAuthenticate_RejectsMissingAndWrongCredentials(t *testing.T) {
	reached := false
	h := open().Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		s := weftapi.GetSession(r)
		if s == nil || s.UserID() != "weft" || s.Org() != 0 {
			t.Errorf("session = %v", s)
		}
	}))
	for _, hdr := range []string{"", basic("weft", "nope"), basic("root", "pw"), "Bearer x"} {
		req := httptest.NewRequest("GET", "/api/functions", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("header %q: %d %q, want 401 asking for Basic", hdr, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		if reached {
			t.Fatalf("header %q reached the handler", hdr)
		}
	}
	req := httptest.NewRequest("GET", "/api/functions", nil)
	req.Header.Set("Authorization", basic("weft", "pw"))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Error("the right credential did not reach the handler")
	}
}

func TestForward_CarriesTheCredentialToTheCoordinator(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", basic("weft", "pw"))
	if got := open().Forward(req)["authorization"]; got != basic("weft", "pw") {
		t.Errorf("forwarded %q", got)
	}
}

// The open build ships no sign-in: the core registers none of those routes and
// this identity adds none, so every one is a 404, and a core route answers an
// anonymous caller 401 asking for the credential.
func TestServer_HasNoLoginRoutesAndGatesTheCore(t *testing.T) {
	srv := weftapi.NewServer(nil, "", "http://localhost")
	srv.SetIdentity(open())
	h := srv.Handler()

	for _, rt := range []struct{ method, path string }{
		{"POST", "/api/auth/login"}, {"POST", "/api/auth/signup"}, {"POST", "/api/auth/verify"},
		{"POST", "/api/auth/device"}, {"GET", "/api/auth/me"}, {"POST", "/api/team/accept"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("")))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: the open build has no such route", rt.method, rt.path, w.Code)
		}
	}
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/functions"}, {"GET", "/api/crons"}, {"GET", "/api/dashboards"}, {"GET", "/api/sources"},
		{"POST", "/api/shell/exec"}, {"GET", "/api/fs/list"}, {"GET", "/api/tokens"}, {"POST", "/api/namespaces"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("")))
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s = %d (%q), want 401 asking for Basic", rt.method, rt.path, w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
}
