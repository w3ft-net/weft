package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TestSeatLimit pins the per-tier seat caps. Used by the api layer
// to reject invites past the cap; if a tier ever rebalances we'd
// want to be deliberate about it.
func TestSeatLimit(t *testing.T) {
	cases := []struct {
		tier Tier
		want int
	}{
		{TierTrial, 1},
		{TierSwatch, 5},
		{TierYard, 10},
		{TierBolt, 50},
		{Tier("nonexistent"), 1}, // unknown tier falls back to trial cap
	}
	for _, c := range cases {
		if got := SeatLimit(c.tier); got != c.want {
			t.Errorf("SeatLimit(%q) = %d, want %d", c.tier, got, c.want)
		}
	}
}

// TestAdvanceOnboardingStep is a compare-and-swap: succeeds only when
// the current step matches the expected value. Mismatch returns
// false-without-error (caller treats as "concurrent advance", not a
// fatal). Pin both branches.
func TestAdvanceOnboardingStep(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("onboard-test@example.com")

	// Fresh user starts at the zero-value step (empty string).
	got, _ := db.GetUserByID(user.ID)
	startStep := got.OnboardingStep

	// Successful advance.
	ok, err := db.AdvanceOnboardingStep(user.ID, startStep, "shell-ls")
	if err != nil {
		t.Fatalf("AdvanceOnboardingStep: %v", err)
	}
	if !ok {
		t.Error("first advance should succeed")
	}

	got, _ = db.GetUserByID(user.ID)
	if got.OnboardingStep != "shell-ls" {
		t.Errorf("OnboardingStep = %q", got.OnboardingStep)
	}

	// Stale advance: expected doesn't match current ("shell-ls"), should
	// return ok=false without erroring.
	ok, err = db.AdvanceOnboardingStep(user.ID, "wrong-expected", "shell-tail")
	if err != nil {
		t.Fatalf("stale advance error: %v", err)
	}
	if ok {
		t.Error("stale advance should return false")
	}

	got, _ = db.GetUserByID(user.ID)
	if got.OnboardingStep != "shell-ls" {
		t.Errorf("stale advance should not change step, got %q", got.OnboardingStep)
	}
}

// TestDismissOnboarding sets the dismissed flag. Used by the "X" on
// the tour overlay; the user can re-enter the tour from the help
// menu, but the overlay won't auto-appear.
func TestDismissOnboarding(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("dismiss-test@example.com")

	if err := db.DismissOnboarding(user.ID); err != nil {
		t.Fatalf("DismissOnboarding: %v", err)
	}
	got, _ := db.GetUserByID(user.ID)
	if !got.OnboardingDismissed {
		t.Error("OnboardingDismissed should be true")
	}
}

// TestSetCanCreateNamespace toggles the namespace-creation grant
// (used by the demo seed and the admin UI).
func TestSetCanCreateNamespace(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("ns-perm-test@example.com")

	if err := db.SetCanCreateNamespace(user.ID, true); err != nil {
		t.Fatalf("SetCanCreateNamespace(true): %v", err)
	}
	got, _ := db.GetUserByID(user.ID)
	if !got.CanCreateNamespace {
		t.Error("CanCreateNamespace should be true")
	}

	if err := db.SetCanCreateNamespace(user.ID, false); err != nil {
		t.Fatalf("SetCanCreateNamespace(false): %v", err)
	}
	got, _ = db.GetUserByID(user.ID)
	if got.CanCreateNamespace {
		t.Error("CanCreateNamespace should be false")
	}
}

// ── TeamMember helpers ─────────────────────────────────────────

// makeTeamMember creates a customer + user + active membership row
// and returns the membership. Used by tests that need an existing
// team_member row to operate on.
func makeTeamMember(t *testing.T, db *Store, customerName, email string) (*Customer, *User, *TeamMember) {
	t.Helper()
	cust, err := db.FindOrCreateCustomer(customerName)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	uid := user.ID
	m := &TeamMember{
		ID:         uuid.New().String(),
		CustomerID: cust.ID,
		UserID:     &uid,
		Email:      email,
		Status:     "active",
		Role:       "member",
		AcceptedAt: &now,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.CreateTeamMember(m); err != nil {
		t.Fatal(err)
	}
	return cust, user, m
}

func TestGetTeamMemberByEmail(t *testing.T) {
	db := testOpen(t)
	cust, _, m := makeTeamMember(t, db, "tm-byemail", "user-byemail@example.com")

	got, err := db.GetTeamMemberByEmail(cust.ID, "user-byemail@example.com")
	if err != nil {
		t.Fatalf("GetTeamMemberByEmail: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("ID = %q, want %q", got.ID, m.ID)
	}

	// Not found
	if _, err := db.GetTeamMemberByEmail(cust.ID, "ghost@example.com"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("expected ErrRecordNotFound for missing email, got %v", err)
	}
}

func TestGetTeamMemberByUserID(t *testing.T) {
	db := testOpen(t)
	cust, user, m := makeTeamMember(t, db, "tm-byuserid", "user-byuserid@example.com")

	got, err := db.GetTeamMemberByUserID(cust.ID, user.ID)
	if err != nil {
		t.Fatalf("GetTeamMemberByUserID: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("ID = %q, want %q", got.ID, m.ID)
	}
}

func TestGetTeamMemberByID(t *testing.T) {
	db := testOpen(t)
	_, _, m := makeTeamMember(t, db, "tm-byid", "user-byid@example.com")

	got, err := db.GetTeamMemberByID(m.ID)
	if err != nil {
		t.Fatalf("GetTeamMemberByID: %v", err)
	}
	if got.Email != m.Email {
		t.Errorf("Email = %q", got.Email)
	}

	// Not found
	if _, err := db.GetTeamMemberByID("does-not-exist"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("expected ErrRecordNotFound, got %v", err)
	}
}

// TestAcceptTeamMember: pending member is converted to active and
// linked to a user_id. AcceptedAt is set.
func TestAcceptTeamMember(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("accept-test")

	// Create a pending invite (no user_id yet).
	pending := &TeamMember{
		ID:         uuid.New().String(),
		CustomerID: cust.ID,
		Email:      "invitee@example.com",
		Status:     "pending",
		Role:       "member",
	}
	if err := db.CreateTeamMember(pending); err != nil {
		t.Fatal(err)
	}

	// User signs up and accepts.
	user, _ := db.CreateUser("invitee@example.com")
	if err := db.AcceptTeamMember(pending.ID, user.ID); err != nil {
		t.Fatalf("AcceptTeamMember: %v", err)
	}

	got, _ := db.GetTeamMemberByID(pending.ID)
	if got.Status != "active" {
		t.Errorf("Status = %q", got.Status)
	}
	if got.UserID == nil || *got.UserID != user.ID {
		t.Errorf("UserID = %v, want %q", got.UserID, user.ID)
	}
	if got.AcceptedAt == nil {
		t.Error("AcceptedAt should be set")
	}
}

// TestRemoveTeamMember soft-removes by setting status. The row sticks
// around (so audit can still see who was removed); list filters
// status != removed.
func TestRemoveTeamMember(t *testing.T) {
	db := testOpen(t)
	cust, _, m := makeTeamMember(t, db, "remove-test", "to-remove@example.com")

	if err := db.RemoveTeamMember(m.ID); err != nil {
		t.Fatalf("RemoveTeamMember: %v", err)
	}

	// Direct fetch still returns the row.
	got, err := db.GetTeamMemberByID(m.ID)
	if err != nil {
		t.Fatalf("GetTeamMemberByID after remove: %v", err)
	}
	if got.Status != "removed" {
		t.Errorf("Status = %q, want removed", got.Status)
	}

	// But filtered fetch skips removed rows.
	if _, err := db.GetTeamMemberByEmail(cust.ID, "to-remove@example.com"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("GetTeamMemberByEmail should skip removed; got %v", err)
	}
}

// TestRemoveTeamMemberByUserAndCustomer covers the onboarding-
// graduation path where the caller has (user, customer) but not the
// membership ID.
func TestRemoveTeamMemberByUserAndCustomer(t *testing.T) {
	db := testOpen(t)
	cust, user, _ := makeTeamMember(t, db, "remove-byuser", "byuser@example.com")

	if err := db.RemoveTeamMemberByUserAndCustomer(user.ID, cust.ID); err != nil {
		t.Fatalf("RemoveTeamMemberByUserAndCustomer: %v", err)
	}

	if _, err := db.GetTeamMemberByUserID(cust.ID, user.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("after remove, GetTeamMemberByUserID should miss; got %v", err)
	}
}

// TestCountTeamMembers counts active + pending, skipping removed
// rows. Used by the seat-cap check before sending invites.
func TestCountTeamMembers(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("count-test")

	// Fresh customer: 0.
	if n, _ := db.CountTeamMembers(cust.ID); n != 0 {
		t.Errorf("initial count = %d", n)
	}

	// Add 3 members of varied status.
	for i, status := range []string{"active", "pending", "active"} {
		now := time.Now()
		m := &TeamMember{
			ID:         uuid.New().String(),
			CustomerID: cust.ID,
			Email:      "count" + string(rune('a'+i)) + "@example.com",
			Status:     status,
			Role:       "member",
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		db.CreateTeamMember(m)
	}
	if n, _ := db.CountTeamMembers(cust.ID); n != 3 {
		t.Errorf("after 3 inserts, count = %d", n)
	}

	// Remove one — should drop to 2.
	var m TeamMember
	db.db.Where("customer_id = ?", cust.ID).First(&m)
	db.RemoveTeamMember(m.ID)
	if n, _ := db.CountTeamMembers(cust.ID); n != 2 {
		t.Errorf("after remove, count = %d", n)
	}
}

