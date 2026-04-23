package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kevlarcon/weft/auth"
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
	tx, rollback := s.BeginTest()
	t.Cleanup(func() {
		rollback()
		s.Close()
	})
	srv := NewServer(tx, "test-secret", "localhost:9020", "http://localhost:3000")
	return srv, tx
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

	// Verify — exchange token for JWT (new users need org_name).
	body, _ = json.Marshal(map[string]string{"token": token, "org_name": "testorg"})
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
	claims, err := auth.Validate(jwt, "test-secret")
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

	// First verify — success (with org_name to consume the link).
	body, _ = json.Marshal(map[string]string{"token": token, "org_name": "oneuse-org"})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first verify failed: %d: %s", w.Code, w.Body.String())
	}

	// Second verify — should fail (used).
	body, _ = json.Marshal(map[string]string{"token": token, "org_name": "oneuse-org"})
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

	cust, _ := s.FindOrCreateCustomer("kevlar")
	user, _ := s.CreateUser(fmt.Sprintf("me-%d@example.com", os.Getpid()), cust.ID)
	jwt, _ := auth.Generate(user, "trial", "test-secret")

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

	cust2, _ := s.FindOrCreateCustomer("kevlar")
	user, _ := s.CreateUser(fmt.Sprintf("queries-%d@example.com", os.Getpid()), cust2.ID)
	jwt, _ := auth.Generate(user, "trial", "test-secret")

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

// ── Email verification gating (security audit follow-up) ───────

func TestAuth_Signup_ReturnsUnverifiedUser(t *testing.T) {
	srv, _ := testServer(t)
	srv.StripePrices["swatch"] = "price_test"
	handler := srv.Handler()
	email := fmt.Sprintf("signup-%d@example.com", os.Getpid())

	body, _ := json.Marshal(map[string]string{
		"email": email, "org_name": fmt.Sprintf("squatter-%d", os.Getpid()), "tier": "swatch",
	})
	req := httptest.NewRequest("POST", "/api/auth/signup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("signup: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	user, _ := resp["user"].(map[string]any)
	if user == nil {
		t.Fatal("no user in signup response")
	}
	if ver, _ := user["email_verified"].(bool); ver != false {
		t.Errorf("signup user should be unverified, got email_verified=%v", user["email_verified"])
	}

	// JWT claim should agree.
	jwt, _ := resp["token"].(string)
	claims, err := auth.Validate(jwt, "test-secret")
	if err != nil {
		t.Fatalf("validate JWT: %v", err)
	}
	if claims.EmailVerified {
		t.Error("JWT claim email_verified should be false on signup")
	}
	if claims.VerificationDeadline == 0 {
		t.Error("JWT should carry a verification_deadline for unverified users")
	}
}

func TestAuth_UnverifiedUser_CannotCreateToken(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("unverified-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("unverified-%d@example.com", os.Getpid()), cust.ID)
	// Owner membership so the isOwner check in handleCreateToken would
	// otherwise pass — we want to be sure the 403 comes from the
	// verification gate, not from permission.
	s.CreateTeamMember(&store.TeamMember{
		ID: "tm-unverified", CustomerName: cust.Name, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
	s.FindOrCreateNamespace(cust.ID, "default")
	jwt, _ := auth.Generate(user, "trial", "test-secret")

	body, _ := json.Marshal(map[string]string{"label": "prod", "namespace": "default"})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("expected 403 for unverified user, got %d: %s", w.Code, w.Body.String())
	}
	var errResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &errResp)
	if errResp["code"] != "email_verification_required" {
		t.Errorf("expected code email_verification_required, got %v", errResp["code"])
	}
}

func TestAuth_MarkVerified_UnlocksTokenCreation(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("verify-unlock-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("verify-unlock-%d@example.com", os.Getpid()), cust.ID)
	s.CreateTeamMember(&store.TeamMember{
		ID: "tm-verify-unlock", CustomerName: cust.Name, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
	s.FindOrCreateNamespace(cust.ID, "default")

	// Flip verified state, then reissue JWT.
	if err := s.MarkUserVerified(user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	user, _ = s.GetUserByID(user.ID)
	jwt, _ := auth.Generate(user, "trial", "test-secret")

	body, _ := json.Marshal(map[string]string{"label": "prod", "namespace": "default"})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201 after verification, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuth_Sweep_DeletesExpiredUnverifiedOnly(t *testing.T) {
	_, s := testServer(t)

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("sweep-%d", os.Getpid()))
	unverifiedEmail := fmt.Sprintf("sweep-unverified-%d@example.com", os.Getpid())
	verifiedEmail := fmt.Sprintf("sweep-verified-%d@example.com", os.Getpid())

	unverified, _ := s.CreateUser(unverifiedEmail, cust.ID)
	_, _ = s.CreateVerifiedUser(verifiedEmail, cust.ID)

	// Cutoff one hour in the future sweeps ALL users whose EmailVerifiedAt
	// is nil regardless of CreatedAt, so we don't have to surgically
	// backdate rows to prove the delete targets the right set.
	deleted, err := s.DeleteExpiredUnverifiedUsers(time.Now().Add(1 * time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deletion (unverified only), got %d", deleted)
	}

	// The unverified user is gone, the verified user survives.
	if _, err := s.GetUserByID(unverified.ID); err == nil {
		t.Error("unverified user should have been deleted")
	}
	if _, err := s.GetUserByEmail(verifiedEmail); err != nil {
		t.Errorf("verified user should still exist: %v", err)
	}
}

func TestNotifyConfig_PutAndGet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateCustomer(fmt.Sprintf("notify-%d", os.Getpid()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("notify-%d@example.com", os.Getpid()), cust.ID)
	s.CreateTeamMember(&store.TeamMember{
		ID: fmt.Sprintf("tm-notify-%d", os.Getpid()), CustomerName: cust.Name, Email: user.Email,
		UserID: &user.ID, Role: "owner", Status: "active",
	})
	jwt, _ := auth.Generate(user, "trial", "test-secret")

	// GET on an untouched customer returns empty yaml, not 404.
	req := httptest.NewRequest("GET", "/api/notify", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET empty: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		AliasesYAML string `json:"aliases_yaml"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.AliasesYAML != "" {
		t.Errorf("expected empty aliases_yaml, got %q", got.AliasesYAML)
	}

	// PUT with missing targets → 400 (validation layer rejects).
	body, _ := json.Marshal(map[string]string{"aliases_yaml": "ops: {}\n"})
	req = httptest.NewRequest("PUT", "/api/notify", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("PUT invalid: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	// PUT with a valid config succeeds, then GET returns exactly what we wrote.
	valid := "ops:\n  slack:\n    - \"#ops\"\n"
	body, _ = json.Marshal(map[string]string{"aliases_yaml": valid})
	req = httptest.NewRequest("PUT", "/api/notify", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT valid: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/notify", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET after PUT: expected 200, got %d", w.Code)
	}
	got.AliasesYAML = ""
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.AliasesYAML != valid {
		t.Errorf("round-trip mismatch:\nwant: %q\ngot:  %q", valid, got.AliasesYAML)
	}
}
