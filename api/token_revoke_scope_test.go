package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Revoking a token must be scoped to the caller's org. The handler used to
// pass the bare primary key to a cross-tenant UPDATE, so any authenticated
// user could revoke another org's token by guessing its id.
func TestRevokeToken_OtherOrgsTokenIsNotRevocable(t *testing.T) {
	srv, s := testServer(t)
	_ = setUpTokenCreator(t, s, "revoke-victim")
	attackerJWT := setUpTokenCreator(t, s, "revoke-attacker")

	victim, err := s.FindOrCreateOrg(fmt.Sprintf("revoke-victim-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := s.FindOrCreateNamespace(victim.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := s.CreateAgentToken(victim.ID, ns.ID, "victim-agent", "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/api/tokens/"+tok.PublicID, nil)
	req.Header.Set("Authorization", "Bearer "+attackerJWT)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("cross-org revoke status = %d, want 404", w.Code)
	}
	if got, err := s.ValidateAgentToken(raw); err != nil || got == nil {
		t.Errorf("victim's token was revoked by another org: result=%v err=%v", got, err)
	}
}

// The list/create responses name a token by its opaque public id and carry
// env; the bigint primary key and the raw-token prefix never appear.
func TestListTokens_ExposesPublicIDAndEnvOnly(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "list-shape")

	w := postJSON(srv, jwt, "/api/tokens", map[string]string{"label": "web", "namespace": "default", "env": "prod"})
	if w.Code != 201 {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	lw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(lw, req)
	var list []map[string]any
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v: %s", err, lw.Body.String())
	}
	if len(list) != 1 {
		t.Fatalf("got %d tokens, want 1", len(list))
	}
	for name, m := range map[string]map[string]any{"create": created, "list": list[0]} {
		id, _ := m["id"].(string)
		if !strings.HasPrefix(id, "tok_") {
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
