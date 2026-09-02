package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
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

// TestCreateToken_ForProducer_ReturnsEndpoint pins the Component 4
// contract: `weft token create --for render` mints a producer-scoped
// token and the response carries the endpoint string the command
// prints (docs/plans/render.md, "Auth (confirmed)").
func TestCreateToken_ForProducer_ReturnsEndpoint(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "token-producer")

	body, _ := json.Marshal(map[string]string{
		"label": "render-prod", "namespace": "default", "producer": "render",
	})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Token    string `json:"token"`
		Producer string `json:"producer"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("empty token")
	}
	if resp.Producer != "render" {
		t.Errorf("producer = %q, want %q", resp.Producer, "render")
	}
	wantEndpoint := srv.syslogEndpoint("render")
	if resp.Endpoint != wantEndpoint {
		t.Errorf("endpoint = %q, want %q", resp.Endpoint, wantEndpoint)
	}

	// The minted token validates scoped to "render" and nowhere else.
	auth, err := s.ValidateAgentTokenForProducer(resp.Token, "render")
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		t.Fatal("minted token did not validate for producer=render")
	}
}

// TestCreateToken_UnknownProducer_Rejected confirms `--for` can't mint
// a token for a made-up platform name — only registered
// ingest/syslog.Producer names are accepted.
func TestCreateToken_UnknownProducer_Rejected(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "token-producer-bad")

	body, _ := json.Marshal(map[string]string{
		"label": "bogus", "namespace": "default", "producer": "not-a-real-platform",
	})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400 for an unregistered producer, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateToken_NoProducer_OmitsEndpoint confirms the ordinary
// weftd/JSON/Heroku token path is unaffected — no producer field in
// the request, no producer/endpoint in the response.
func TestCreateToken_NoProducer_OmitsEndpoint(t *testing.T) {
	srv, s := testServer(t)
	jwt := setUpTokenCreator(t, s, "token-no-producer")

	body, _ := json.Marshal(map[string]string{"label": "plain", "namespace": "default"})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["producer"]; ok {
		t.Errorf("unexpected producer field in response: %v", resp["producer"])
	}
	if _, ok := resp["endpoint"]; ok {
		t.Errorf("unexpected endpoint field in response: %v", resp["endpoint"])
	}
}
