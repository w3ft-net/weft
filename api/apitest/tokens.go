package apitest

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/source"
)

// Tokens runs the routes that create, list and revoke a namespace's tokens and
// the namespaces they belong to.
func Tokens(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"NamespaceMustExist", tokenNamespaceMustExist},
		{"NamespaceListIsRejected", tokenNamespaceListRejected},
		{"CreateNamespaceRejectsNamesThatCannotBeOne", createNamespaceNames},
		{"OtherTenantsTokenIsNotRevocable", revokeOtherTenantsToken},
		{"ListExposesPublicIDAndEnvOnly", listTokensShape},
		{"RemoveNamespace", removeNamespace},
	})
}

func (s Subject) namespaceNames(t *testing.T) map[string]bool {
	t.Helper()
	var list struct {
		Namespaces []struct {
			Name string `json:"name"`
		} `json:"namespaces"`
	}
	decode(t, s.Do("GET", "/api/namespaces", nil), &list)
	out := map[string]bool{}
	for _, n := range list.Namespaces {
		out[n.Name] = true
	}
	return out
}

// A token is minted for ONE existing namespace. `--namespace acme,globex` was
// accepted and silently created a namespace literally named "acme,globex"; a
// typo silently created that namespace too. The gRPC CreateToken already refused
// both; the HTTP handler behind the CLI and the web form did not.
func tokenNamespaceMustExist(t *testing.T, s Subject) {
	s.namespace(t, "default")
	w := s.post(t, "/api/tokens", map[string]string{"label": "x", "namespace": "defualt"})
	if w.Code != 404 {
		t.Fatalf("typo'd namespace: status %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create it first") {
		t.Errorf("error should say to create the namespace first: %s", w.Body.String())
	}
	if got := s.namespaceNames(t); got["defualt"] || len(got) != 1 || !got["default"] {
		t.Errorf("token create must not create namespaces; have %v", got)
	}
}

func tokenNamespaceListRejected(t *testing.T, s Subject) {
	s.namespace(t, "default")
	for _, ns := range []string{"default,other", "default other", "default/prod", "default+tokens"} {
		w := s.post(t, "/api/tokens", map[string]string{"label": "x", "namespace": ns})
		if w.Code != 400 {
			t.Errorf("namespace %q: status %d, want 400: %s", ns, w.Code, w.Body.String())
			continue
		}
		if !strings.Contains(w.Body.String(), "one namespace") {
			t.Errorf("namespace %q: error should say a token takes one namespace: %s", ns, w.Body.String())
		}
	}
	if got := s.namespaceNames(t); len(got) != 1 {
		t.Errorf("rejected requests created namespaces: %v", got)
	}
}

// The create-namespace form shares the name rule, so a name that could never be
// a single token scope can't be created in the first place.
func createNamespaceNames(t *testing.T, s Subject) {
	for _, name := range []string{"a,b", "a b", "a/b", "a+tokens", "a*", "-flag"} {
		if w := s.post(t, "/api/namespaces", map[string]string{"name": name}); w.Code != 400 {
			t.Errorf("create %q: status %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	if w := s.post(t, "/api/namespaces", map[string]string{"name": "production"}); w.Code != 201 {
		t.Errorf("create production: status %d, want 201: %s", w.Code, w.Body.String())
	}
}

// Revoking a token is scoped to the caller's tenant. The handler once passed the
// bare primary key to a cross-tenant UPDATE, so any authenticated caller could
// revoke another tenant's token by guessing its id.
func revokeOtherTenantsToken(t *testing.T, s Subject) {
	s.needsOther(t)
	ns, err := s.Sources.FindOrCreateNamespace(s.Other, "default")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := s.Tokens.CreateToken(s.Other, ns.ID, "victim-agent", "")
	if err != nil {
		t.Fatal(err)
	}
	w := s.Do("DELETE", "/api/tokens/"+tok.PublicID, nil)
	if w.Code != 404 {
		t.Errorf("cross-tenant revoke status = %d, want 404", w.Code)
	}
	if got, err := s.Tokens.ValidateToken(raw); err != nil || got == nil {
		t.Errorf("the other tenant's token was revoked: result=%v err=%v", got, err)
	}
}

// The list and create responses name a token by its opaque public id and carry
// env; the primary key and the raw-token prefix never appear.
func listTokensShape(t *testing.T, s Subject) {
	s.namespace(t, "default")
	w := s.post(t, "/api/tokens", map[string]string{"label": "web", "namespace": "default", "env": "prod"})
	if w.Code != 201 {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	decode(t, w, &created)
	var list []map[string]any
	decode(t, s.Do("GET", "/api/tokens", nil), &list)
	if len(list) != 1 {
		t.Fatalf("got %d tokens, want 1", len(list))
	}
	for name, m := range map[string]map[string]any{"create": created, "list": list[0]} {
		if id, _ := m["id"].(string); !strings.HasPrefix(id, "tok_") {
			t.Errorf("%s: id = %v, want tok_ public id", name, m["id"])
		}
		if _, ok := m["prefix"]; ok {
			t.Errorf("%s: response still carries prefix", name)
		}
		if m["env"] != "prod" {
			t.Errorf("%s: env = %v, want prod", name, m["env"])
		}
	}
	if list[0]["id"] != created["id"] {
		t.Errorf("list id %v != create id %v", list[0]["id"], created["id"])
	}
}

// DELETE /api/namespaces/{name}: an empty namespace goes; one with sources or
// live tokens is refused with a 409 that names them, unless ?force=true, which
// removes it all.
func removeNamespace(t *testing.T, s Subject) {
	del := func(path string) *httptest.ResponseRecorder { return s.Do("DELETE", path, nil) }
	exists := func(name string) bool {
		_, ok, err := s.Sources.GetNamespace(s.Owner, name)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	s.namespace(t, "empty")
	if w := del("/api/namespaces/empty"); w.Code != 204 {
		t.Fatalf("delete of an empty namespace = %d: %s", w.Code, w.Body.String())
	}
	if exists("empty") {
		t.Error("namespace still exists after delete")
	}
	if w := del("/api/namespaces/empty"); w.Code != 404 {
		t.Errorf("deleting a missing namespace = %d, want 404", w.Code)
	}

	full := s.namespace(t, "full")
	if err := s.Sources.SubmitSample(full, "api", source.SampleSubmission{
		Instance: "h", Deployment: "d", SampleJSON: `["x"]`, DetectionJSON: "{}", Collected: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Tokens.CreateToken(s.Owner, full, "t", ""); err != nil {
		t.Fatal(err)
	}
	w := del("/api/namespaces/full")
	if w.Code != 409 {
		t.Fatalf("delete of a non-empty namespace = %d, want 409: %s", w.Code, w.Body.String())
	}
	var refusal struct {
		Sources []string `json:"sources"`
		Tokens  int      `json:"tokens"`
	}
	decode(t, w, &refusal)
	if len(refusal.Sources) != 1 || refusal.Sources[0] != "api" || refusal.Tokens != 1 {
		t.Errorf("409 body = %s, want sources [api] and 1 token", w.Body.String())
	}
	if !exists("full") {
		t.Fatal("a refused delete removed the namespace")
	}
	if w := del("/api/namespaces/full?force=true"); w.Code != 204 {
		t.Fatalf("forced delete = %d: %s", w.Code, w.Body.String())
	}
	if exists("full") {
		t.Error("forced delete left the namespace behind")
	}
}
