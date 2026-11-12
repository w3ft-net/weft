package hostedapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

func TestAuth_LoginAndVerify(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	email := uniqueEmail(t)

	// Login requires an existing account.
	if _, err := s.CreateUser(email); err != nil {
		t.Fatalf("create user: %v", err)
	}

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

	// Verify JWT is valid. Identity fields (email, tier) live on the
	// server now; the JWT only carries the user ID.
	claims, err := auth.Validate(jwt, "test-secret")
	if err != nil {
		t.Fatalf("validate JWT: %v", err)
	}
	if claims.UserID == "" {
		t.Errorf("expected non-empty user ID")
	}
}

func TestAuth_MagicLinkOneTimeUse(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	email := fmt.Sprintf("oneuse-%d@example.com", os.Getpid())

	// Login requires an existing account.
	if _, err := s.CreateUser(email); err != nil {
		t.Fatalf("create user: %v", err)
	}

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

	cust, _ := s.FindOrCreateOrg("kevlar")
	user, _ := s.CreateUser(fmt.Sprintf("me-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	jwt, _ := auth.Generate(user, "test-secret")

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

func TestAuth_Sweep_DeletesExpiredUnverifiedOnly(t *testing.T) {
	_, s := testServer(t)

	if _, err := s.FindOrCreateOrg(fmt.Sprintf("sweep-%d", os.Getpid())); err != nil {
		t.Fatal(err)
	}
	unverifiedEmail := fmt.Sprintf("sweep-unverified-%d@example.com", os.Getpid())
	verifiedEmail := fmt.Sprintf("sweep-verified-%d@example.com", os.Getpid())

	unverified, _ := s.CreateUser(unverifiedEmail)
	_, _ = s.CreateVerifiedUser(verifiedEmail)

	// Cutoff one hour in the future sweeps ALL users whose EmailVerifiedAt
	// is nil regardless of CreatedAt, so we don't have to surgically
	// backdate rows to prove the delete targets the right set.
	deleted, err := func() (int64, error) {
		result := s.DB().
			Where("email_verified_at IS NULL AND created_at < ?", time.Now().Add(1*time.Hour)).
			Delete(&store.User{})
		return result.RowsAffected, result.Error
	}()
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

func TestOnboarding_StartTour_KeepsUserLoggedIn(t *testing.T) {
	srv, s := testServer(t)
	srv.tierPurchasable = func(t store.Tier) bool { return t == store.TierSwatch }
	handler := srv.Handler()

	// Seed Loom (customer + namespace) so grantLoomMembership at signup
	// has somewhere to land. In prod this is weft-demo-seed's job; here
	// we set up just what the onboarding code reads.
	loomCust, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom customer: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loomCust.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	// Sign up the user via the public endpoints (not a hand-rolled fixture
	// — we want every side effect signup and verify have).
	email := fmt.Sprintf("tour-%d@example.com", os.Getpid())
	orgName := fmt.Sprintf("tour-org-%d", os.Getpid())
	body, _ := json.Marshal(map[string]string{
		"email": email, "org_name": orgName, "tier": "swatch",
	})
	req := httptest.NewRequest("POST", "/api/auth/signup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("signup: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The session comes from following the emailed link, which is also what
	// creates the account.
	linkToken, err := s.GetLastMagicLinkToken(email)
	if err != nil {
		t.Fatalf("signup sent no link: %v", err)
	}
	body, _ = json.Marshal(map[string]string{"token": linkToken})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("verify: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var verifyResp map[string]any
	json.Unmarshal(w.Body.Bytes(), &verifyResp)
	jwt, _ := verifyResp["token"].(string)
	if jwt == "" {
		t.Fatal("verify: no JWT in response")
	}

	// First /me: confirms the welcome-modal preconditions. This is what
	// the SPA sees right after PaymentSetup redirects to /dashboards.
	req = httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("me-before: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var meBefore map[string]any
	json.Unmarshal(w.Body.Bytes(), &meBefore)
	if step, _ := meBefore["onboarding_step"].(string); step != "welcome" {
		t.Fatalf("expected onboarding_step=welcome, got %q", step)
	}
	var hasLoom bool
	for _, o := range meBefore["orgs"].([]any) {
		if o.(map[string]any)["name"] == "loom" {
			hasLoom = true
		}
	}
	if !hasLoom {
		t.Fatal("fresh signup is missing loom membership (welcome modal's Start button would be disabled)")
	}

	// Step 2 — the modal's first server hop: POST /api/onboarding/advance.
	body, _ = json.Marshal(map[string]string{"step": "welcome"})
	req = httptest.NewRequest("POST", "/api/onboarding/advance", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("advance: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Step 3 — switchOrg sets weft_org=loom and reloads. AuthContext's
	// post-reload /me carries X-Active-Org: loom. If this 4xxs the SPA
	// boots the login screen (token gets cleared in the catch handler).
	req = httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-Active-Org", "loom")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("me-after-switchOrg: expected 200, got %d: %s\n(this is the bug — the SPA treats a non-200 here as logged-out and shows the login screen)",
			w.Code, w.Body.String())
	}
	var meAfter map[string]any
	json.Unmarshal(w.Body.Bytes(), &meAfter)
	if cust, _ := meAfter["customer"].(string); cust != "loom" {
		t.Errorf("active customer after switchOrg: want loom, got %q", cust)
	}
	if step, _ := meAfter["onboarding_step"].(string); step != "downloads" {
		t.Errorf("onboarding_step after advance: want downloads, got %q", step)
	}
}
