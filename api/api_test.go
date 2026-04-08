package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func testServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := NewServer(store, "test-secret", "localhost:9020", "http://localhost:3000")
	return srv, store
}

func TestAuth_LoginAndVerify(t *testing.T) {
	srv, store := testServer(t)
	handler := srv.Handler()

	// Login — request magic link.
	body, _ := json.Marshal(map[string]string{"email": "test@example.com"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("login: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Get the magic link token from the store.
	var link MagicLink
	store.db.Where("email = ?", "test@example.com").First(&link)
	if link.Token == "" {
		t.Fatal("no magic link created")
	}

	// Verify — exchange token for JWT.
	body, _ = json.Marshal(map[string]string{"token": link.Token})
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
	if claims.Email != "test@example.com" {
		t.Errorf("expected email test@example.com, got %s", claims.Email)
	}
	if claims.Tier != "trial" {
		t.Errorf("expected tier trial, got %s", claims.Tier)
	}
}

func TestAuth_MagicLinkOneTimeUse(t *testing.T) {
	srv, store := testServer(t)
	handler := srv.Handler()

	// Create magic link.
	body, _ := json.Marshal(map[string]string{"email": "test@example.com"})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var link MagicLink
	store.db.Where("email = ?", "test@example.com").First(&link)

	// First verify — success.
	body, _ = json.Marshal(map[string]string{"token": link.Token})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first verify failed: %d", w.Code)
	}

	// Second verify — should fail (used).
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("second verify should fail, got %d", w.Code)
	}
}

func TestAuth_Me(t *testing.T) {
	srv, store := testServer(t)
	handler := srv.Handler()

	// Create user and get JWT.
	user, _ := store.CreateUser("me@example.com", "kevlar")
	jwt, _ := GenerateJWT(user, "test-secret")

	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("me: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["email"] != "me@example.com" {
		t.Errorf("expected me@example.com, got %v", resp["email"])
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
	srv, store := testServer(t)
	handler := srv.Handler()

	user, _ := store.CreateUser("queries@example.com", "kevlar")
	jwt, _ := GenerateJWT(user, "test-secret")

	// Create.
	body, _ := json.Marshal(map[string]string{"name": "error check", "query": "cat vimgym.log | grep ERROR"})
	req := httptest.NewRequest("POST", "/api/queries", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var created SavedQuery
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

	var queries []SavedQuery
	json.Unmarshal(w.Body.Bytes(), &queries)
	if len(queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(queries))
	}

	// Delete.
	req = httptest.NewRequest("DELETE", "/api/queries/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("delete: expected 204, got %d", w.Code)
	}

	// Verify deleted.
	remaining, _ := store.ListSavedQueries(user.ID)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 queries after delete, got %d", len(remaining))
	}
}
