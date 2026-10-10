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

// The browser reports every completed shell line (with its exit code) to
// /api/shell/builtin so the tour advances from what the user actually ran,
// not from whatever pipeline text the server happened to see: the client
// strips pipe stages like `| chart ...` and runs inline builtins like
// `history` locally, so neither ever reached the server's exec-path hook.
// A failed command must not advance a lesson.
func TestShellBuiltin_ExitCodeGatesLessonAdvance(t *testing.T) {
	srv, s := testServer(t)
	handler := srv.Handler()
	jwt := superadminJWT(t, s)

	loom, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loom.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	email := fmt.Sprintf("builtin-owner-%d@acme.example.com", os.Getpid())
	body, _ := json.Marshal(map[string]string{"email": email, "org_name": "BuiltinCo", "tier": "bolt", "reason": "t"})
	req := httptest.NewRequest("POST", "/api/admin/customers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	user, err := s.GetUserByEmail(email)
	if err != nil || user == nil {
		t.Fatalf("user: %v", err)
	}
	userJWT, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	setStep := func(step string) {
		t.Helper()
		if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).Update("onboarding_step", step).Error; err != nil {
			t.Fatalf("set step: %v", err)
		}
	}
	stepNow := func() string {
		t.Helper()
		u, err := s.GetUserByID(user.ID)
		if err != nil {
			t.Fatalf("get user: %v", err)
		}
		return u.OnboardingStep
	}
	report := func(payload map[string]any) int {
		t.Helper()
		b, _ := json.Marshal(payload)
		r := httptest.NewRequest("POST", "/api/shell/builtin", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+userJWT)
		r.Header.Set("X-Active-Org", LoomCustomerSlug)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}

	setStep("shell-history")

	if code := report(map[string]any{"pipeline": "history", "exit_code": 1}); code != 204 {
		t.Errorf("failed command: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-history" {
		t.Fatalf("a failed command advanced the lesson to %q", got)
	}

	if code := report(map[string]any{"pipeline": "history", "exit_code": 0}); code != 200 {
		t.Errorf("clean command: status %d, want 200", code)
	}
	if got := stepNow(); got == "shell-history" {
		t.Errorf("a clean `history` did not advance the lesson")
	}

	// No exit_code at all keeps the pre-existing behavior (older clients).
	setStep("shell-history")
	if code := report(map[string]any{"pipeline": "history"}); code != 200 {
		t.Errorf("legacy report without exit_code: status %d, want 200", code)
	}

	// Stopping a live tail reports exit 130 (SIGINT). For the tail -f lesson
	// that stop is the completion; for any other lesson an interrupt is not.
	setStep("shell-tail-f")
	if code := report(map[string]any{"pipeline": "tail -f /log/loom/prod/api/main.log", "exit_code": 130}); code != 200 {
		t.Errorf("stopping the live tail: status %d, want 200", code)
	}
	if got := stepNow(); got == "shell-tail-f" {
		t.Errorf("stopping the live tail did not complete the tail -f lesson")
	}

	setStep("shell-history")
	if code := report(map[string]any{"pipeline": "history", "exit_code": 130}); code != 204 {
		t.Errorf("interrupted history: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-history" {
		t.Fatalf("an interrupted command advanced the lesson to %q", got)
	}
}
