package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/kevlarcon/weft/store"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	return dsn
}

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dsn := testDSN(t)
	s, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	srv := NewServer(s, "test-secret", "localhost:9020", "http://localhost:3000")
	return srv, s
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d@example.com", os.Getpid())
}

func TestAuth_LoginAndVerify(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	email := uniqueEmail(t)

	// Login — request magic link.
	body, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("login: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Get the magic link token from the store.
	token, _ := s.GetLastMagicLinkToken(email)
	if token == "" {
		t.Fatal("no magic link created")
	}

	// Verify — exchange token for JWT.
	body, _ = json.Marshal(map[string]string{"token": token})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("verify: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	jwt, ok := resp["token"].(string)
	if !ok || jwt == "" {
		t.Fatal("no JWT in response")
	}

	// Verify JWT is valid.
	claims, err := ValidateJWT(jwt, "test-secret")
	if err != nil {
		t.Fatalf("validate JWT: %v", err)
	}
	if claims.Email != email {
		t.Errorf("expected email %s, got %s", email, claims.Email)
	}
	if claims.Tier != "trial" {
		t.Errorf("expected tier trial, got %s", claims.Tier)
	}
}

func TestAuth_MagicLinkOneTimeUse(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	email := fmt.Sprintf("oneuse-%d@example.com", os.Getpid())

	// Create magic link.
	body, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	token, _ := s.GetLastMagicLinkToken(email)

	// First verify — success.
	body, _ = json.Marshal(map[string]string{"token": token})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first verify failed: %d", w.Code)
	}

	// Second verify — should fail (used).
	body, _ = json.Marshal(map[string]string{"token": token})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("second verify should fail, got %d", w.Code)
	}
}

func TestAuth_Me(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	user, _ := s.CreateUser(fmt.Sprintf("me-%d@example.com", os.Getpid()), "kevlar")
	jwt, _ := GenerateJWT(user, "trial", "test-secret")

	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("me: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["email"] != user.Email {
		t.Errorf("expected %s, got %v", user.Email, resp["email"])
	}
}

func TestAuth_Unauthorized(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.Handler()

	// No auth header.
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Bad token.
	req = httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 for bad token, got %d", w.Code)
	}
}

func TestSavedQueries_CRUD(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	user, _ := s.CreateUser(fmt.Sprintf("queries-%d@example.com", os.Getpid()), "kevlar")
	jwt, _ := GenerateJWT(user, "trial", "test-secret")

	// Create.
	body, _ := json.Marshal(map[string]string{"name": "error check", "query": "cat vimgym.log | grep ERROR"})
	req := httptest.NewRequest("POST", "/api/queries", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created store.SavedQuery
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Name != "error check" {
		t.Errorf("expected name 'error check', got %s", created.Name)
	}

	// List.
	req = httptest.NewRequest("GET", "/api/queries", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list: expected 200, got %d", w.Code)
	}

	var queries []store.SavedQuery
	json.Unmarshal(w.Body.Bytes(), &queries)
	if len(queries) < 1 {
		t.Fatalf("expected at least 1 query, got %d", len(queries))
	}

	// Delete.
	req = httptest.NewRequest("DELETE", "/api/queries/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("delete: expected 204, got %d", w.Code)
	}
}
