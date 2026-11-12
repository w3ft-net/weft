//go:build !defaultshape

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/alerts"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

func testDSN(t *testing.T) string {
	t.Helper()
	return store.TestDSN(t)
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
	srv := NewServer(tx, "localhost:9020", "http://localhost:3000")
	srv.SetIdentity(jwtIdentity{tx})
	srv.SetTenancy(hosted.NewAdapter(tx))
	srv.SetAlertKinds(alerting.NewRegistry(tx, alerts.Targets(tx, nil)...)) // what hostedapi installs
	return srv, tx
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d@example.com", os.Getpid())
}

// TestWatchCreate_TierFromDB_NotJWT pins the regression that comp grants
// (or any tier change) propagate without a re-login. We mint a JWT
// while the customer is on trial, then upgrade them to swatch in the
// DB out-of-band, then use the OLD trial JWT to create a watch.
// Pre-fix the request would fail with "watches are not available on
// the trial plan" because handleSaveWatch read tier from claims.
func TestWatchCreate_TierFromDB_NotJWT(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg("tier-from-db")
	email := fmt.Sprintf("tdb-%d@example.com", os.Getpid())
	user, _ := s.CreateUser(email)
	now := time.Now()
	verified := now
	user.EmailVerifiedAt = &verified
	markUserVerified(s, user.ID)
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)

	// Subscription minted while the customer is on trial.
	trialEnd := now.Add(14 * 24 * time.Hour)
	s.CreateSubscription(&store.Subscription{
		ID:        "trial-" + cust.Name,
		OrgID:     cust.ID,
		Tier:      string(store.TierTrial),
		Status:    "active",
		StartsAt:  now,
		ExpiresAt: &trialEnd,
	})
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}

	// Comp-grant out-of-band — JWT now stale.
	if err := s.GrantCompSubscription(cust.ID, string(store.TierBolt), "test-grant"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.FindOrCreateNamespace(cust.ID, "x"); err != nil {
		t.Fatal(err)
	}

	// Use the stale JWT to create a watch. Should succeed because the
	// handler reads tier from the live DB (now bolt = 50 watches).
	body := []byte(`{"schedule":"*/5 * * * *","pipeline":"echo 1"}`)
	req := httptest.NewRequest("PUT", "/api/crons/x/test-watch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 && w.Code != 201 {
		t.Errorf("watch create with stale-trial JWT but live-bolt DB: got %d %s",
			w.Code, w.Body.String())
	}
}

// setupWatchOwner creates a verified, bolt-tier user + org and
// returns a JWT for saving watches — the shared boilerplate for the
// SaveWatch error/warning tests below.
func setupWatchOwner(t *testing.T, s *store.Store, orgName string) string {
	t.Helper()
	cust, _ := s.FindOrCreateOrg(orgName)
	user, err := s.CreateUser(fmt.Sprintf("%s-%d@example.com", orgName, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	markUserVerified(s, user.ID)
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	// Crons live in a namespace; these tests keep theirs in "x".
	if _, err := s.FindOrCreateNamespace(cust.ID, "x"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expires := now.Add(365 * 24 * time.Hour)
	if err := s.CreateSubscription(&store.Subscription{
		ID:        "bolt-" + cust.Name,
		OrgID:     cust.ID,
		Tier:      string(store.TierBolt),
		Status:    "active",
		StartsAt:  now,
		ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	jwt, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	return jwt
}

// TestSaveWatch_SyntaxErrorSurfacesRealMessage pins the fix for a
// standing bug: handleSaveWatch used to discard SaveCronWatch's error
// entirely and return a generic "internal error" 500, so a bash
// syntax error typed into a cron pipeline via the web UI or MCP
// (which both go through this endpoint) was reported as a mystery
// server failure instead of the real, actionable message. See
// docs/plans/cron-truncation-lint.md, "Generic warning mechanism".
func TestSaveWatch_SyntaxErrorSurfacesRealMessage(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := setupWatchOwner(t, s, "syntax-err-watch")

	body := []byte(`{"schedule":"*/5 * * * *","pipeline":"cat |"}`)
	req := httptest.NewRequest("PUT", "/api/crons/x/broken", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("bad pipeline: got %d, want 400 (bad request): %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no following command") {
		t.Errorf("response body should carry the real parse-error message, got: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "internal error") {
		t.Errorf("response body should not be the old generic message, got: %q", w.Body.String())
	}
}

// TestSaveWatch_TruncationWarning: a pipeline shaped like a real
// truncation risk (log-pattern-count's raw expansion) still saves
// (200), and the response body carries the warning under a generic
// "warnings" field — the same shape any future save-time check would
// use, not something specific to this one.
func TestSaveWatch_TruncationWarning(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := setupWatchOwner(t, s, "truncation-warn-watch")

	body := []byte(`{"schedule":"*/5 * * * *","pipeline":"tail -n 1000 /log/x/y/main.log | grep -c \"boom\""}`)
	req := httptest.NewRequest("PUT", "/api/crons/x/risky", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("truncation-risk pipeline: got %d, want 200 (warn, don't block): %s", w.Code, w.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
	// The truncation lint, then the explanation of the grep -c stage.
	if len(resp.Warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v (body: %s)", len(resp.Warnings), resp.Warnings, w.Body.String())
	}
	if !strings.Contains(resp.Warnings[0], "tail -n 1000") {
		t.Errorf("warning text should name the cap, got: %q", resp.Warnings[0])
	}

	saved, err := s.GetCronWatch(func() store.OrgID { cust, _ := s.FindOrCreateOrg("truncation-warn-watch"); return cust.ID }(), xNS(t, s, func() store.OrgID { cust, _ := s.FindOrCreateOrg("truncation-warn-watch"); return cust.ID }()), "risky")

	if err != nil {
		t.Fatalf("GetCronWatch: %v (want the watch to have actually saved)", err)
	}
	if saved.Name != "risky" {
		t.Errorf("saved watch name: got %q, want risky", saved.Name)
	}
}

// TestWriteFS_CronPathWithWarning_StillSucceeds: the generic
// multi-path file editor (PUT /api/fs/write) also routes /cron.d/
// writes through SaveCronWatch. Before store.Split, any non-nil
// error there — including a non-blocking store.Warning — was
// reported as a 400. A truncation-risk cron written through this
// endpoint must still succeed (204), even though this generic
// surface has nowhere to display the warning text itself.
func TestWriteFS_CronPathWithWarning_StillSucceeds(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := setupWatchOwner(t, s, "writefs-cron-warn")

	body := []byte(`{"path":"/cron.d/x/risky","content":"# schedule: */5 * * * *\ntail -n 1000 /log/x/y/main.log | grep -c \"boom\"\n"}`)
	req := httptest.NewRequest("PUT", "/api/fs/write", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("cron write with a truncation-risk pipeline: got %d, want 204: %s", w.Code, w.Body.String())
	}

	cust, _ := s.FindOrCreateOrg("writefs-cron-warn")
	saved, err := s.GetCronWatch(cust.ID, xNS(t, s, cust.ID), "risky")
	if err != nil {
		t.Fatalf("GetCronWatch: %v (want the watch to have actually saved)", err)
	}
	if saved.Name != "risky" {
		t.Errorf("saved watch name: got %q, want risky", saved.Name)
	}
}

func TestAuth_Unauthorized(t *testing.T) {
	srv, _ := testServer(t)
	handler := srv.Handler()

	// No auth header.
	req := httptest.NewRequest("GET", "/api/functions", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Bad token.
	req = httptest.NewRequest("GET", "/api/functions", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("expected 401 for bad token, got %d", w.Code)
	}
}

// ── Email verification gating (security audit follow-up) ───────

func TestAuth_UnverifiedUser_CannotCreateToken(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("unverified-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("unverified-%d@example.com", os.Getpid()))
	// Owner membership so the isOwner check in handleCreateToken would
	// otherwise pass — we want to be sure the 403 comes from the
	// verification gate, not from permission.
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	s.FindOrCreateNamespace(cust.ID, "default")
	jwt, _ := auth.Generate(user, "test-secret")

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

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("verify-unlock-%d", os.Getpid()))
	user, _ := s.CreateUser(fmt.Sprintf("verify-unlock-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	s.FindOrCreateNamespace(cust.ID, "default")

	// Flip verified state, then reissue JWT.
	if err := markUserVerified(s, user.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	user, _ = s.GetUserByID(user.ID)
	jwt, _ := auth.Generate(user, "test-secret")

	body, _ := json.Marshal(map[string]string{"label": "prod", "namespace": "default"})
	req := httptest.NewRequest("POST", "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201 after verification, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetFunction_ReturnsPlainSource(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("getfn-%d", os.Getpid()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("getfn-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	// GetByID hydrates user.CustomerName via the Customer join — auth.Generate
	// reads it directly into the JWT's customer claim.
	user, _ = s.GetUserByID(user.ID)
	jwt, _ := auth.Generate(user, "test-secret")

	source := "cat /log/api.log | grep ERROR | wc -l"
	if err := s.SaveShellFunction(cust.ID, user.ID, "scan", source, "bash"); err != nil {
		t.Fatalf("save fn: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/functions/scan", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET function: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	body := strings.TrimRight(w.Body.String(), "\n")
	if body != source {
		t.Errorf("body should be the script source, got %q\nwant: %q", body, source)
	}
	// Spot-check that we did not get the JSON-encoded ShellFunction
	// struct (the bug surfaced as `cat /bin/scan` printing
	// `{"ID":...,"Source":"...",...}`).
	if strings.HasPrefix(body, "{") && strings.Contains(body, `"Source"`) {
		t.Errorf("body looks JSON-encoded, want plain source: %q", body)
	}
}

func TestNotifyConfig_PutAndGet(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()

	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notify-%d", os.Getpid()))
	user, _ := s.CreateVerifiedUser(fmt.Sprintf("notify-%d@example.com", os.Getpid()))
	s.AddTeamMemberWithEmail(cust.ID, user.ID, user.Email, store.RoleOwner)
	jwt, _ := auth.Generate(user, "test-secret")

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
	// A new org's only alias is the system one, pointed at its owner.
	if aliases, err := store.ParseNotifyAliases(got.AliasesYAML); err != nil || len(aliases) != 1 || aliases[store.SystemAlias] == nil {
		t.Errorf("expected only the seeded system alias, got %q (%v)", got.AliasesYAML, err)
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
	valid := "system:\n  email:\n    - \"oncall@example.test\"\nops:\n  slack:\n    - \"#ops\"\n"
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

// TestOnboarding_StartTour_KeepsUserLoggedIn reproduces the flow the
// welcome modal's "Start the tour" button drives:
//
//  1. Fresh signup → server lands user with own org + Loom membership,
//     onboarding_step="welcome".
//  2. Client POSTs /api/onboarding/advance with step=welcome (modal's
//     advanceOnboarding call).
//  3. Client sets weft_org=loom in localStorage and reloads — on the
//     next /me, the request carries X-Active-Org: loom.
//
// The user reported that this final /me returns a state that makes the
// SPA render the login page (anything non-2xx triggers the AuthContext
// catch that wipes the token). The pin is: that /me must come back 200.

// markUserVerified inlines the former store.MarkUserVerified, which was
// relocated into production callers. Stamps email_verified_at if unset.
func markUserVerified(s *store.Store, userID string) error {
	now := time.Now()
	return s.DB().Model(&store.User{}).
		Where("id = ? AND email_verified_at IS NULL", userID).
		Updates(map[string]any{
			"email_verified_at": &now,
			"updated_at":        now,
		}).Error
}

// xNS is the id of namespace "x", where these tests keep their crons.
func xNS(t *testing.T, s *store.Store, org store.OrgID) uint {
	t.Helper()
	ns, err := s.FindOrCreateNamespace(org, "x")
	if err != nil {
		t.Fatal(err)
	}
	return ns.ID
}
