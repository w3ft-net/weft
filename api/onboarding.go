package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/coordinator/onboarding"
	"github.com/kevlarcon/weft/store"
)

// LoomCustomerSlug is the slug of the populated demo org every signup
// gets membership in. The producer containers in
// docker-compose.demo.yml feed this customer; weft-demo-seed
// pre-populates its bin/, cron.d/, dashboards.
const LoomCustomerSlug = "loom"

// LoomDemoNamespace is the namespace inside Loom that signups get
// CanRead on. Onboarding lessons walk the user through paths under
// /log/<this>/...
const LoomDemoNamespace = "loom"

// onboardingSequence is the full ordered list of onboarding_step
// values. The non-shell prefix (welcome → panel tour) is hardcoded
// here because it's tied to UI elements that live in the frontend;
// the shell-* middle comes from the lesson package (canonical
// source); "done" terminates. Renaming or reordering shell lessons
// in coordinator/onboarding/lessons.go automatically reflows the
// state machine.
var onboardingSequence = buildOnboardingSequence()

func buildOnboardingSequence() []string {
	prefix := []string{"welcome", "downloads", "namespaces", "dashboards"}
	shell := onboarding.Steps()
	out := make([]string, 0, len(prefix)+len(shell)+1)
	out = append(out, prefix...)
	out = append(out, shell...)
	out = append(out, "done")
	return out
}

// nextOnboardingStep returns the step that follows current. Returns
// "" if current isn't in the sequence (caller should reject) or if
// current == "done" (already graduated, no further step exists).
func nextOnboardingStep(current string) string {
	for i, step := range onboardingSequence {
		if step == current && i+1 < len(onboardingSequence) {
			return onboardingSequence[i+1]
		}
	}
	return ""
}

// grantLoomMembership is called during user creation to give a
// fresh user a read-only membership in the Loom demo org. The
// welcome modal (and everything downstream) keys off this row's
// presence, so signup is the only place this happens.
//
// Soft-fails: if Loom isn't seeded in this environment (local dev
// before docker-compose.demo.yml is up, or a deploy where we
// haven't run weft-demo-seed yet), grantLoomMembership logs and
// returns nil — the user signs up successfully and their welcome
// modal will just point at an org they can't switch into. Better
// than failing the signup outright.
func (s *Server) grantLoomMembership(userID, email string) error {
	cust, err := s.store.GetCustomerByName(LoomCustomerSlug)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("onboarding: Loom customer not seeded; skipping membership grant for %s", email)
			return nil
		}
		return err
	}
	ns, err := s.store.GetNamespace(cust.ID, LoomDemoNamespace)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("onboarding: Loom namespace not seeded; skipping namespace permission for %s", email)
			// Still create the team_member row — the user can at
			// least switch into Loom; no readable namespaces yet
			// just means they see a quiet org.
		} else {
			return err
		}
	}

	now := time.Now()
	if err := s.store.CreateTeamMember(&store.TeamMember{
		ID:         uuid.New().String(),
		CustomerID: cust.ID,
		Email:      email,
		UserID:     &userID,
		Role:       "member",
		Status:     "active",
		InvitedAt:  now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		return err
	}
	if ns != nil {
		if err := s.store.UpsertNamespacePermission(&store.NamespacePermission{
			UserID:      userID,
			NamespaceID: ns.ID,
			CanRead:     true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// graduateOnboarding revokes the user's Loom membership at the end
// of the tour and cleans up Loom-scoped scratch state (saved
// queries). Called from handleOnboardingAdvance when the step
// transitions to 'done'. Idempotent — re-running on an already-
// graduated user is a no-op.
func (s *Server) graduateOnboarding(userID string) error {
	cust, err := s.store.GetCustomerByName(LoomCustomerSlug)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // Loom wasn't seeded; nothing to revoke
		}
		return err
	}

	if err := s.store.DeleteSavedQueriesForUserInCustomer(cust.ID, userID); err != nil {
		log.Printf("onboarding: clean Loom saved_queries for %s: %v", userID, err)
		// Non-fatal — better to graduate than to leave the user
		// stuck because of a cleanup hiccup.
	}

	if err := s.store.RemoveTeamMemberByUserAndCustomer(userID, cust.ID); err != nil {
		return err
	}

	// Find the user's home org (oldest remaining active membership)
	// and pin it as last_active so the next request lands them
	// somewhere coherent rather than 403'ing.
	memberships, err := s.store.ListUserMemberships(userID)
	if err == nil && len(memberships) > 0 {
		_ = s.store.UpdateUserLastActiveCustomer(userID, memberships[0].CustomerID)
	}

	// Graduation banner is gated on (step==done && !HasSeenNotice).
	// Marking happens when the user dismisses the banner via
	// handleGraduationNoticeSeen — not here.
	return nil
}

// GraduationNoticeKey is the user_notices key for the one-shot
// "you've graduated, go set up your own org" banner that shows on
// the home org after a user finishes the Loom tour. Exported so the
// /me handler and the dismiss endpoint stay aligned.
const GraduationNoticeKey = "onboarding_graduated"

// handleGraduationNoticeSeen flips the user_notices row for the
// graduation banner so it doesn't render on subsequent loads. One-
// shot; idempotent.
func (s *Server) handleGraduationNoticeSeen(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if err := s.store.MarkNoticeSeen(session.UserID, GraduationNoticeKey, "success"); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOnboardingAdvance transitions the user's onboarding_step
// from `expected` to the next step in the sequence. Idempotent on
// race — two clients advancing the same step land one update; the
// other gets a 200 with the current state.
//
// When the transition ends at 'done', also runs graduation.
func (s *Server) handleOnboardingAdvance(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	var req struct {
		Expected string `json:"step"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Expected == "" {
		http.Error(w, "step required", http.StatusBadRequest)
		return
	}
	next := nextOnboardingStep(req.Expected)
	if next == "" {
		http.Error(w, "invalid step", http.StatusBadRequest)
		return
	}

	advanced, err := s.store.AdvanceOnboardingStep(session.UserID, req.Expected, next)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if advanced && next == "done" {
		if err := s.graduateOnboarding(session.UserID); err != nil {
			log.Printf("onboarding: graduate %s: %v", session.UserID, err)
			http.Error(w, "graduation failed", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"step":     next,
		"advanced": advanced,
	})
}

// handleOnboardingDismiss flips onboarding_dismissed=true. The user
// keeps their Loom membership (so they can poke at it via the org
// switcher) but the welcome modal and tour overlays go quiet.
func (s *Server) handleOnboardingDismiss(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if err := s.store.DismissOnboarding(session.UserID); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleShellBuiltin receives pipeline strings from builtins that
// executed locally in the WASM shell (ls, cat /etc/..., etc.) and
// checks them against onboarding predicates. Without this, commands
// handled entirely client-side never reach maybeAdvanceShellLesson.
func (s *Server) handleShellBuiltin(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	var req struct {
		Pipeline string `json:"pipeline"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pipeline == "" {
		http.Error(w, "pipeline required", http.StatusBadRequest)
		return
	}
	if newStep, prompt, advanced := s.maybeAdvanceShellLesson(session, req.Pipeline); advanced {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"step":   newStep,
			"prompt": prompt,
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maybeAdvanceShellLesson is the shell-side hook into the onboarding
// state machine. handleShellExec calls it after every successful
// pipeline completion (EOF on the gRPC stream). If the user is in a
// shell-* lesson AND the just-run pipeline matches the lesson's
// predicate, it advances the step and returns the new step + the
// next lesson's prompt for the SSE/CLI banner to render. Returns
// advanced=false (with empty step/prompt) for any of: not in Loom,
// dismissed, no current shell lesson, predicate didn't match,
// or store error (logged).
func (s *Server) maybeAdvanceShellLesson(session *auth.Session, pipeline string) (newStep, prompt string, advanced bool) {
	if session == nil || session.ActiveCustomerName != LoomCustomerSlug {
		log.Printf("onboarding: skip — active org is %q, not loom", session.ActiveCustomerName)
		return "", "", false
	}
	user, err := s.store.GetUserByID(session.UserID)
	if err != nil || user == nil {
		log.Printf("onboarding: skip — user lookup failed: %v", err)
		return "", "", false
	}
	if user.OnboardingDismissed {
		log.Printf("onboarding: skip — dismissed")
		return "", "", false
	}
	current := user.OnboardingStep
	log.Printf("onboarding: step=%q pipeline=%q match=%v", current, pipeline, onboarding.CheckPipeline(current, pipeline))
	if !onboarding.CheckPipeline(current, pipeline) {
		return "", "", false
	}
	next := nextOnboardingStep(current)
	if next == "" {
		return "", "", false
	}
	advanced, err = s.store.AdvanceOnboardingStep(user.ID, current, next)
	if err != nil {
		log.Printf("onboarding: shell advance %s→%s for %s: %v", current, next, user.ID, err)
		return "", "", false
	}
	if !advanced {
		return "", "", false
	}
	if next == "done" {
		if err := s.graduateOnboarding(user.ID); err != nil {
			log.Printf("onboarding: graduate from shell hook for %s: %v", user.ID, err)
		}
	}
	return next, onboarding.PromptFor(next), true
}
