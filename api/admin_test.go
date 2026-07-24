package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
)

// superadminJWT creates a superadmin user and returns a JWT for it. Admin
// endpoints gate on session.IsSuperadmin, which AuthMiddleware materializes
// from the users row — so the flag has to be persisted, not just claimed.
func superadminJWT(t *testing.T, s *store.Store) string {
	t.Helper()
	email := fmt.Sprintf("super-%d@example.com", os.Getpid())
	user, err := s.CreateUser(email)
	if err != nil {
		t.Fatalf("create superadmin: %v", err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).
		Update("is_superadmin", true).Error; err != nil {
		t.Fatalf("promote superadmin: %v", err)
	}
	user.IsSuperadmin = true

	// BuildSession (via AuthMiddleware) needs the caller to belong to at
	// least one org — a superadmin is still a real user somewhere.
	cust, err := s.FindOrCreateCustomer(fmt.Sprintf("super-org-%d", os.Getpid()))
	if err != nil {
		t.Fatalf("superadmin org: %v", err)
	}
	if err := s.CreateTeamMember(&store.TeamMember{
		ID:         fmt.Sprintf("super-mem-%d", os.Getpid()),
		CustomerID: cust.ID,
		Email:      email,
		UserID:     &user.ID,
		Role:       "owner",
		Status:     "active",
	}); err != nil {
		t.Fatalf("superadmin membership: %v", err)
	}

	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	return jwt
}

// TestMe_SuperadminWithoutMembership pins the real-world logout bug: the
// production superadmin (paul@graybeardcorp.com) belongs to no customer
// org. BuildSession returned ErrNoMembership for such a user, so
// AuthMiddleware 403'd every authed request — including GET /api/auth/me.
// The SPA treats any non-2xx /me as logged-out (it wipes the token in
// AuthContext's catch), so the first hard navigation after login (e.g.
// typing /admin) booted the operator straight back to the login screen.
//
// A superadmin is a platform operator who may legitimately have zero org
// memberships; /me must still come back 200 so the session survives.
func TestMe_SuperadminWithoutMembership(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	// Superadmin with NO team membership and no last_active_customer_id —
	// exactly the production operator's shape.
	email := fmt.Sprintf("super-nomem-%d@example.com", os.Getpid())
	user, err := s.CreateVerifiedUser(email)
	if err != nil {
		t.Fatalf("create superadmin: %v", err)
	}
	if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).
		Update("is_superadmin", true).Error; err != nil {
		t.Fatalf("promote superadmin: %v", err)
	}
	user.IsSuperadmin = true

	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("me for membership-less superadmin: expected 200, got %d: %s\n(this is the bug — a non-200 here makes the SPA wipe the token and show the login screen)",
			w.Code, w.Body.String())
	}

	var me map[string]any
	json.Unmarshal(w.Body.Bytes(), &me)
	if su, _ := me["is_superadmin"].(bool); !su {
		t.Error("expected is_superadmin=true in /me for the operator")
	}
	// No org to scope to — customer is empty and the org list is bare.
	if cust, _ := me["customer"].(string); cust != "" {
		t.Errorf("expected empty active customer for org-less superadmin, got %q", cust)
	}
}

func TestAdminCreateCustomer(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	ownerEmail := fmt.Sprintf("owner-%d@acme.example.com", os.Getpid())
	body, _ := json.Marshal(map[string]string{
		"email":    ownerEmail,
		"org_name": "AcmeCo",
		"tier":     "bolt",
		"reason":   "design-partner",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Name       string `json:"name"`
		Tier       string `json:"tier"`
		CompReason string `json:"comp_reason"`
		OwnerEmail string `json:"owner_email"`
		LoginURL   string `json:"login_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "acmeco" { // normalized to lowercase
		t.Errorf("name = %q, want %q", resp.Name, "acmeco")
	}
	if resp.LoginURL == "" {
		t.Error("expected a login_url for the operator to hand off")
	}

	// Customer exists.
	cust, err := s.GetCustomerByName("acmeco")
	if err != nil {
		t.Fatalf("customer not created: %v", err)
	}

	// Owner user exists and is email-verified (not gated).
	owner, err := s.GetUserByEmail(ownerEmail)
	if err != nil || owner == nil {
		t.Fatalf("owner user not created: %v", err)
	}
	if owner.EmailVerifiedAt == nil {
		t.Error("owner should be created email-verified")
	}

	// Owner membership is active with role owner.
	members, err := s.ListTeamMembers(cust.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 1 || members[0].Role != "owner" || members[0].Status != "active" {
		t.Fatalf("expected one active owner membership, got %+v", members)
	}

	// Active comp subscription at the requested tier.
	sub, err := s.ActiveSubscription(cust.ID)
	if err != nil || sub == nil {
		t.Fatalf("no active subscription: %v", err)
	}
	if sub.Tier != "bolt" || !sub.IsComp() || sub.CompReason != "design-partner" {
		t.Errorf("subscription = %+v, want comp bolt/design-partner", sub)
	}
}

// TestAdminCreateCustomer_GrantsLoomMembership pins the comp-account
// onboarding regression: the retired public-signup path called
// grantLoomMembership so a fresh user landed with a read-only seat in
// the Loom demo org, which is the precondition the welcome/tour modal
// keys off (WelcomeModal enables "Start the tour" only when the user's
// orgs include loom). handleAdminCreateCustomer — the operator
// replacement for signup — never made that grant, so comp accounts
// joined with no Loom membership and a permanently-disabled tour.
func TestAdminCreateCustomer_GrantsLoomMembership(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	// Seed Loom (customer + namespace) so grantLoomMembership has
	// somewhere to land. In prod this is weft-demo-seed's job.
	loomCust, err := s.FindOrCreateCustomer(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom customer: %v", err)
	}
	loomNS, err := s.FindOrCreateNamespace(loomCust.ID, LoomDemoNamespace)
	if err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	ownerEmail := fmt.Sprintf("comp-owner-%d@acme.example.com", os.Getpid())
	body, _ := json.Marshal(map[string]string{
		"email":    ownerEmail,
		"org_name": "CompCo",
		"tier":     "bolt",
		"reason":   "design-partner",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	owner, err := s.GetUserByEmail(ownerEmail)
	if err != nil || owner == nil {
		t.Fatalf("owner user not created: %v", err)
	}

	// The comp owner must hold an active membership in Loom — otherwise
	// the welcome modal's "Start the tour" button is disabled forever.
	memberships, err := s.ListUserMemberships(owner.ID)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	var inLoom bool
	for _, m := range memberships {
		if m.Customer.Name == LoomCustomerSlug && m.Status == "active" {
			inLoom = true
		}
	}
	if !inLoom {
		t.Fatalf("comp owner has no active Loom membership (welcome modal's Start button would be disabled); memberships=%+v", memberships)
	}

	// And read access to the Loom demo namespace so the tour has data.
	perms, err := s.ListUserNamespacePermissions(owner.ID)
	if err != nil {
		t.Fatalf("list ns perms: %v", err)
	}
	var canReadLoom bool
	for _, p := range perms {
		if p.NamespaceID == loomNS.ID && p.CanRead {
			canReadLoom = true
		}
	}
	if !canReadLoom {
		t.Errorf("comp owner lacks CanRead on the Loom demo namespace; perms=%+v", perms)
	}
}

// TestCompAccountVerify_ShowsWelcomeModal pins the trigger the user
// reported missing: after a comp account owner clicks their magic link
// (POST /api/auth/verify), the response feeds AuthContext's setUser
// directly, and the welcome modal renders iff that user object carries
// onboarding_step=="welcome" and an orgs list containing loom.
//
// The verify response used to return a 4-field subset (id/email/tier/
// email_verified) — no onboarding_step, no orgs — so the modal's gate
// (`user.onboarding_step !== 'welcome'` against undefined) never fired
// and the comp owner landed on an empty dashboard with no tour. The fix
// is that /verify returns the same user shape as /me.
func TestCompAccountVerify_ShowsWelcomeModal(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	loomCust, err := s.FindOrCreateCustomer(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom customer: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loomCust.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	// Operator provisions the comp account and gets back a magic-link URL.
	ownerEmail := fmt.Sprintf("comp-verify-%d@acme.example.com", os.Getpid())
	body, _ := json.Marshal(map[string]string{
		"email": ownerEmail, "org_name": "VerifyCo", "tier": "bolt",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create comp: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		LoginURL string `json:"login_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.LoginURL == "" {
		t.Fatal("no login_url returned")
	}
	u, err := url.Parse(created.LoginURL)
	if err != nil {
		t.Fatalf("parse login_url %q: %v", created.LoginURL, err)
	}
	token := u.Query().Get("token")
	if token == "" {
		t.Fatalf("login_url has no token: %q", created.LoginURL)
	}

	// The owner clicks the link — Verify.tsx POSTs the token to /verify,
	// and AuthContext seeds its user object from the response.
	body, _ = json.Marshal(map[string]string{"token": token})
	req = httptest.NewRequest("POST", "/api/auth/verify", bytes.NewReader(body))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("verify: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Token string `json:"token"`
		User  struct {
			OnboardingStep string `json:"onboarding_step"`
			Orgs           []struct {
				Name string `json:"name"`
			} `json:"orgs"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode verify response: %v", err)
	}
	if resp.Token == "" {
		t.Error("verify response missing JWT")
	}
	if resp.User.OnboardingStep != "welcome" {
		t.Errorf("verify response user.onboarding_step = %q, want %q — the welcome modal's gate reads this field off the object AuthContext seeds from /verify; anything but 'welcome' means the modal never renders on the first session",
			resp.User.OnboardingStep, "welcome")
	}
	var hasLoom bool
	for _, o := range resp.User.Orgs {
		if o.Name == LoomCustomerSlug {
			hasLoom = true
		}
	}
	if !hasLoom {
		t.Errorf("verify response user.orgs %+v does not include loom — the modal's 'Start the tour' button stays disabled", resp.User.Orgs)
	}
}

func TestAdminCreateCustomer_DuplicateOrg(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	if _, err := s.FindOrCreateCustomer("taken"); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"email":    fmt.Sprintf("dup-%d@example.com", os.Getpid()),
		"org_name": "taken",
		"tier":     "bolt",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 409 {
		t.Fatalf("duplicate org: expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminCreateCustomer_RequiresSuperadmin(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	// A normal (non-superadmin) user.
	user, err := s.CreateUser(fmt.Sprintf("plain-%d@example.com", os.Getpid()))
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	jwt, _ := auth.Generate(user, "test-secret")

	body, _ := json.Marshal(map[string]string{
		"email":    fmt.Sprintf("nope-%d@example.com", os.Getpid()),
		"org_name": "shouldnotexist",
		"tier":     "bolt",
	})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Fatalf("non-superadmin: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := s.GetCustomerByName("shouldnotexist"); err == nil {
		t.Error("customer should not have been created by a non-superadmin")
	}
}
