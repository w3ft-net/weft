package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/graybeardcorp/saaskit/orgs"
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

	if err := db.db.Model(&User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"onboarding_dismissed": true,
		"updated_at":           time.Now(),
	}).Error; err != nil {
		t.Fatalf("DismissOnboarding: %v", err)
	}
	got, _ := db.GetUserByID(user.ID)
	if !got.OnboardingDismissed {
		t.Error("OnboardingDismissed should be true")
	}
}

// TestNsmgrGroup toggles the namespace-creation grant, which lives on
// org_permissions as the nsmgr group rather than a global user flag —
// the grant authorizes an org-scoped action, so it has to be per-org
// (migration 070).
func TestNsmgrGroup(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("nsmgr-%d", pid))
	alice := fmt.Sprintf("alice-nsmgr-%d@x.test", pid)
	_ = addTeamUser(t, db, cust.ID, alice, "member")

	if err := db.AddUserToGroup(cust.ID, alice, "nsmgr"); err != nil {
		t.Fatalf("add nsmgr: %v", err)
	}
	perm, err := db.GetOrgPermission(cust.ID, userIDForEmail(t, db, cust.ID, alice))
	if err != nil {
		t.Fatalf("GetOrgPermission after grant: %v", err)
	}
	if !perm.CanCreateNamespaces {
		t.Error("CanCreateNamespaces should be true after nsmgr grant")
	}

	if err := db.RemoveUserFromGroup(cust.ID, alice, "nsmgr"); err != nil {
		t.Fatalf("remove nsmgr: %v", err)
	}
	perm, err = db.GetOrgPermission(cust.ID, userIDForEmail(t, db, cust.ID, alice))
	if err != nil {
		t.Fatalf("GetOrgPermission after revoke: %v", err)
	}
	if perm.CanCreateNamespaces {
		t.Error("CanCreateNamespaces should be false after nsmgr revoke")
	}
}

// userIDForEmail resolves a member's user ID within an org.
func userIDForEmail(t *testing.T, db *Store, orgID OrgID, email string) string {
	t.Helper()
	m, err := db.GetTeamMemberByEmail(orgID, email)
	if err != nil {
		t.Fatalf("GetTeamMemberByEmail(%s): %v", email, err)
	}
	return m.UserID
}

// ── TeamMember helpers ─────────────────────────────────────────

// makeTeamMember creates a customer + user + active membership row
// and returns the membership. Used by tests that need an existing
// team_member row to operate on.
func makeTeamMember(t *testing.T, db *Store, customerName, email string) (*Organization, *User, *TeamMember) {
	t.Helper()
	cust, err := db.FindOrCreateOrg(customerName)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(email)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddTeamMemberWithEmail(cust.ID, user.ID, email, orgs.RoleMember); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetTeamMemberByUserID(cust.ID, user.ID)
	if err != nil {
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

	got, err := getTeamMemberByID(db, m.ID)
	if err != nil {
		t.Fatalf("GetTeamMemberByID: %v", err)
	}
	if got.Email != m.Email {
		t.Errorf("Email = %q", got.Email)
	}

	// Not found
	if _, err := getTeamMemberByID(db, "00000000-0000-0000-0000-0000000000ff"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("expected ErrRecordNotFound, got %v", err)
	}
}

// TestAcceptTeamMember: pending member is converted to active and
// linked to a user_id. AcceptedAt is set.
func TestAcceptTeamMember(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("accept-test")

	// Create a pending invite (no user_id yet).
	pending, err := db.InviteTeamMember(cust.ID, "invitee@example.com", orgs.RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}

	// User signs up and accepts.
	user, _ := db.CreateUser("invitee@example.com")
	if _, err := db.AcceptTeamMember(pending.ID, user.ID); err != nil {
		t.Fatalf("AcceptTeamMember: %v", err)
	}

	got, _ := getTeamMemberByID(db, pending.ID)
	if got.Status != "active" {
		t.Errorf("Status = %q", got.Status)
	}
	if got.UserID != user.ID {
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

	if err := db.RemoveTeamMemberByEmail(cust.ID, m.Email); err != nil {
		t.Fatalf("RemoveTeamMemberByEmail: %v", err)
	}

	// Direct fetch still returns the row.
	got, err := getTeamMemberByID(db, m.ID)
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

	if err := db.scoped(cust.ID).Model(&TeamMember{}).
		Where("user_id = ?", user.ID).
		Updates(map[string]any{
			"status":     "removed",
			"updated_at": time.Now(),
		}).Error; err != nil {
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
	cust, _ := db.FindOrCreateOrg("count-test")

	// Fresh customer: 0.
	if n, _ := db.CountTeamMembers(cust.ID); n != 0 {
		t.Errorf("initial count = %d", n)
	}

	// Add 3 members of varied status: an outstanding invite holds a seat
	// just as an accepted member does, so all three count.
	for i, status := range []string{"active", "pending", "active"} {
		email := "count" + string(rune('a'+i)) + "@example.com"
		if status == "pending" {
			if _, err := db.InviteTeamMember(cust.ID, email, orgs.RoleMember, ""); err != nil {
				t.Fatal(err)
			}
			continue
		}
		u, err := db.CreateUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddTeamMemberWithEmail(cust.ID, u.ID, email, orgs.RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := db.CountTeamMembers(cust.ID); n != 3 {
		t.Errorf("after 3 inserts, count = %d", n)
	}

	// Remove one — should drop to 2.
	if err := db.RemoveTeamMemberByEmail(cust.ID, "counta@example.com"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountTeamMembers(cust.ID); n != 2 {
		t.Errorf("after remove, count = %d", n)
	}
}

// --- helpers for team-member store methods relocated out of the store package ---
//
// Production logic moved to callers outside the store package. These
// test-local helpers reproduce the original method bodies verbatim
// (parameterized on db) so the team tests keep exercising the same
// behaviour.

// getTeamMemberByID fetches a membership by its own id, including removed
// ones — the tests assert on post-removal state, which every orgs read
// deliberately filters out. Raw SQL because that visibility is exactly what
// the library declines to expose.
func getTeamMemberByID(db *Store, id string) (*TeamMember, error) {
	var m TeamMember
	var email, userID, invitedBy *string
	row := db.DB().Raw(
		`SELECT id, org_id, email, user_id, role, status, invited_by, accepted_at
		   FROM memberships WHERE id = ?`, id).Row()
	if err := row.Scan(&m.ID, &m.OrgID, &email, &userID, &m.Role, &m.Status, &invitedBy, &m.AcceptedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, gorm.ErrRecordNotFound
		}
		return nil, err
	}
	if email != nil {
		m.Email = *email
	}
	if userID != nil {
		m.UserID = *userID
	}
	if invitedBy != nil {
		m.InvitedBy = *invitedBy
	}
	return &m, nil
}
