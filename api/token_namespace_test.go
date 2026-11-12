//go:build !defaultshape

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// setUpTokenCreator returns a ready owner JWT for a fresh org with a
// "default" namespace — the common setup every handleCreateToken test
// needs.
func setUpTokenCreator(t *testing.T, s *store.Store, orgPrefix string) string {
	t.Helper()
	cust, err := s.FindOrCreateOrg(fmt.Sprintf("%s-%d", orgPrefix, os.Getpid()))
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	user, err := s.CreateVerifiedUser(fmt.Sprintf("%s-%d@example.com", orgPrefix, os.Getpid()))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	if _, err := s.FindOrCreateNamespace(cust.ID, "default"); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	return jwt
}

func postJSON(srv *Server, jwt, path string, body map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func namespaceNames(t *testing.T, srv *Server, jwt string) map[string]bool {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/namespaces", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var list struct {
		Namespaces []struct {
			Name string `json:"name"`
		} `json:"namespaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode namespaces: %v: %s", err, w.Body.String())
	}
	out := map[string]bool{}
	for _, n := range list.Namespaces {
		out[n.Name] = true
	}
	return out
}

// A token is minted for ONE existing namespace. `--namespace acme,globex`
// was accepted and silently created a namespace literally named
// "acme,globex"; a typo ("acmee") silently created that namespace too.
// The gRPC CreateToken already refused both ("not found — create it
// first"); the HTTP handler behind the CLI and the web form did not.
func TestCreateToken_NamespaceMustExist(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "token-ns-missing")

	w := postJSON(srv, jwt, "/api/tokens", map[string]string{"label": "x", "namespace": "defualt"})
	if w.Code != 404 {
		t.Fatalf("typo'd namespace: status %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create it first") {
		t.Errorf("error should say to create the namespace first: %s", w.Body.String())
	}
	if got := namespaceNames(t, srv, jwt); got["defualt"] || len(got) != 1 || !got["default"] {
		t.Errorf("token create must not create namespaces; have %v", got)
	}
}

func TestCreateToken_NamespaceListIsRejected(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "token-ns-list")

	for _, ns := range []string{"default,other", "default other", "default/prod", "default+tokens"} {
		w := postJSON(srv, jwt, "/api/tokens", map[string]string{"label": "x", "namespace": ns})
		if w.Code != 400 {
			t.Errorf("namespace %q: status %d, want 400: %s", ns, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), "one namespace") {
			t.Errorf("namespace %q: error should say a token takes one namespace: %s", ns, w.Body.String())
		}
	}
	if got := namespaceNames(t, srv, jwt); len(got) != 1 {
		t.Errorf("rejected requests created namespaces: %v", got)
	}
}

// The create-namespace form shares the name rule, so a name that could
// never be a single token scope can't be created in the first place.
func TestCreateNamespace_RejectsNamesThatCannotBeOne(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "ns-create-names")

	for _, name := range []string{"a,b", "a b", "a/b", "a+tokens", "a*", "-flag"} {
		if w := postJSON(srv, jwt, "/api/namespaces", map[string]string{"name": name}); w.Code != 400 {
			t.Errorf("create %q: status %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	if w := postJSON(srv, jwt, "/api/namespaces", map[string]string{"name": "production"}); w.Code != 201 {
		t.Errorf("create production: status %d, want 201: %s", w.Code, w.Body.String())
	}
}

// Granting a permission on a namespace that doesn't exist is a 404, not a
// silent creation. The handler already answered "namespace not found", but
// got there through FindOrCreateNamespace, which creates it.
