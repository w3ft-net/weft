package hostedapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// The browser reports every completed shell line (with its exit code) to
// /api/shell/builtin so the tour advances from what the user actually ran,
// not from whatever pipeline text the server happened to see: the client
// strips pipe stages like `| chart ...` and runs inline builtins like
// `history` locally, so neither ever reached the server's exec-path hook.
// A failed command must not advance a lesson.
func builtinHarness(t *testing.T) (setStep func(string), stepNow func() string, report func(map[string]any) int) {
	t.Helper()
	srv, s := testServer(t)
	handler := srv.Handler()

	loom, err := s.FindOrCreateOrg(LoomCustomerSlug)
	if err != nil {
		t.Fatalf("seed loom: %v", err)
	}
	if _, err := s.FindOrCreateNamespace(loom.ID, LoomDemoNamespace); err != nil {
		t.Fatalf("seed loom namespace: %v", err)
	}

	email := fmt.Sprintf("builtin-owner-%d@acme.example.com", os.Getpid())
	user := seedCompOwner(t, srv, s, email, "BuiltinCo", "bolt", "t")
	userJWT, err := auth.Generate(user, "test-secret")
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}

	setStep = func(step string) {
		t.Helper()
		if err := s.DB().Model(&store.User{}).Where("id = ?", user.ID).Update("onboarding_step", step).Error; err != nil {
			t.Fatalf("set step: %v", err)
		}
	}
	stepNow = func() string {
		t.Helper()
		u, err := s.GetUserByID(user.ID)
		if err != nil {
			t.Fatalf("get user: %v", err)
		}
		return u.OnboardingStep
	}
	report = func(payload map[string]any) int {
		t.Helper()
		b, _ := json.Marshal(payload)
		r := httptest.NewRequest("POST", "/api/shell/builtin", bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+userJWT)
		r.Header.Set("X-Active-Org", LoomCustomerSlug)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	return setStep, stepNow, report
}

func TestShellBuiltin_ExitCodeGatesLessonAdvance(t *testing.T) {
	setStep, stepNow, report := builtinHarness(t)

	setStep("shell-usage")

	if code := report(map[string]any{"pipeline": "cat /etc/usage", "exit_code": 1}); code != 204 {
		t.Errorf("failed command: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-usage" {
		t.Fatalf("a failed command advanced the lesson to %q", got)
	}

	if code := report(map[string]any{"pipeline": "cat /etc/usage", "exit_code": 0}); code != 200 {
		t.Errorf("clean command: status %d, want 200", code)
	}
	if got := stepNow(); got == "shell-usage" {
		t.Errorf("a clean `cat /etc/usage` did not advance the lesson")
	}

	// No exit_code at all keeps the pre-existing behavior (older clients).
	setStep("shell-usage")
	if code := report(map[string]any{"pipeline": "cat /etc/usage"}); code != 200 {
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

	setStep("shell-usage")
	if code := report(map[string]any{"pipeline": "cat /etc/usage", "exit_code": 130}); code != 204 {
		t.Errorf("interrupted cat: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-usage" {
		t.Fatalf("an interrupted command advanced the lesson to %q", got)
	}
}

// `trace --id <id>` opens a modal. Finishing the command must not move the tour on
// (the next prefill would land under the open modal); closing the modal does.
func TestShellBuiltin_TraceLessonAdvancesOnModalClose(t *testing.T) {
	setStep, stepNow, report := builtinHarness(t)
	const line = "trace --id 0123456789abcdef0123456789abcdef"

	setStep("shell-trace")
	if code := report(map[string]any{"pipeline": line, "exit_code": 0}); code != 204 {
		t.Errorf("command completion: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-trace" {
		t.Fatalf("completing `trace` advanced the lesson to %q before the modal closed", got)
	}

	if code := report(map[string]any{"pipeline": line, "exit_code": 1, "event": "modal_closed"}); code != 204 {
		t.Errorf("modal closed after a failed load: status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-trace" {
		t.Fatalf("a failed trace advanced the lesson to %q", got)
	}

	if code := report(map[string]any{"pipeline": line, "exit_code": 0, "event": "modal_closed"}); code != 200 {
		t.Errorf("modal closed: status %d, want 200", code)
	}
	if got := stepNow(); got == "shell-trace" {
		t.Errorf("closing the trace modal did not advance the lesson")
	}
}

// A modal-closed report never advances a lesson that isn't about a modal.
func TestShellBuiltin_ModalCloseDoesNotAdvanceOtherLessons(t *testing.T) {
	setStep, stepNow, report := builtinHarness(t)
	setStep("shell-usage")
	if code := report(map[string]any{"pipeline": "cat /etc/usage", "exit_code": 0, "event": "modal_closed"}); code != 204 {
		t.Errorf("status %d, want 204", code)
	}
	if got := stepNow(); got != "shell-usage" {
		t.Fatalf("modal_closed advanced a non-modal lesson to %q", got)
	}
}
