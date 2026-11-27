package local

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/shape"
)

func testIdentity() *Identity {
	return &Identity{Basic: defaults.Basic{User: "weft", Password: "pw"}}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestFromEnv_RefusesToStartUnconfigured(t *testing.T) {
	t.Setenv(defaults.BasicUserEnv, "")
	t.Setenv(defaults.BasicPasswordEnv, "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("an open api with no credential started")
	}
}

func TestAuthenticate_RejectsMissingAndWrongCredentials(t *testing.T) {
	reached := false
	h := testIdentity().Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		s := weftapi.GetSession(r)
		if s == nil || s.UserID() != "weft" || s.Principal().Partition() != (shape.Added{}) {
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
	if got := testIdentity().Forward(req)["authorization"]; got != basic("weft", "pw") {
		t.Errorf("forwarded %q", got)
	}
}

// The open build ships no sign-in: the core registers none of those routes and
// this identity adds only /api/auth/me, so every sign-in route is a 404, and a core route answers an
// anonymous caller 401 asking for the credential.
func TestServer_HasNoLoginRoutesAndGatesTheCore(t *testing.T) {
	srv := weftapi.NewServer("", "http://localhost")
	srv.SetIdentity(testIdentity())
	h := srv.Handler()

	for _, rt := range []struct{ method, path string }{
		{"POST", "/api/auth/login"}, {"POST", "/api/auth/signup"}, {"POST", "/api/auth/verify"},
		{"POST", "/api/auth/device"}, {"POST", "/api/team/accept"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("")))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: the open build has no such route", rt.method, rt.path, w.Code)
		}
	}
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/auth/me"}, {"GET", "/api/functions"}, {"GET", "/api/crons"}, {"GET", "/api/sources"},
		{"POST", "/api/shell/exec"}, {"GET", "/api/fs/list"}, {"GET", "/api/tokens"}, {"POST", "/api/namespaces"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("")))
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s = %d (%q), want 401 asking for Basic", rt.method, rt.path, w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
}

// The web app learns it is in an open session from /api/auth/me: there is no
// token to hold and no sign-in page to show.
func TestMe_SaysTheSessionIsOpen(t *testing.T) {
	srv := weftapi.NewServer("", "http://localhost")
	srv.SetIdentity(testIdentity())
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", basic("weft", "pw"))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var me map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["session"] != "open" || me["email"] != "weft" {
		t.Errorf("me = %v", me)
	}
}
