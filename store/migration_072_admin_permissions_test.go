package store

import (
	"fmt"
	"os"
	"testing"

	"gorm.io/gorm"
)

// Migration 072's membership trigger is the sole writer of admin-derived
// permission state. These tests drive it directly.
//
// The demotion cases go through raw SQL rather than a store method
// because weft has no role-change path: memberships.role is written once
// at insert and never updated. What varies for a user is which
// membership is active (the org switcher sets X-Active-Org and
// BuildSession resolves the row for that org) — a user can be an admin
// in one org and a plain member in another without any role ever
// changing. The demotion branch exists for the day that path is built,
// and for operators doing it by hand today; these tests keep it honest
// in the meantime.

// demote flips a membership's role to member, optionally announcing the
// operator via the transaction-local GUC the trigger reads.
//
// Both statements must run on the SAME connection: set_config(..., true)
// is transaction-local, so issuing it against the pooled *sql.DB would
// land it on a different connection and the trigger would see nothing —
// silently, with no error and no notice. That is the failure mode this
// helper exists to make impossible to reproduce by accident.
func demote(t *testing.T, db *Store, orgID OrgID, userID string, actingUserID string) {
	t.Helper()
	err := db.DB().Transaction(func(tx *gorm.DB) error {
		if actingUserID != "" {
			err := tx.Exec(`SELECT set_config('weft.acting_user_id', ?, true)`, actingUserID).Error
			if err != nil {
				return err
			}
		}
		return tx.Exec(
			`UPDATE memberships SET role = 'member' WHERE org_id = ? AND user_id = ?`,
			orgID, userID).Error
	})
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
}

// TestTrigger_PromotionMaterializesGrants: becoming an active admin
// creates the row that every read now depends on.
func TestTrigger_PromotionMaterializesGrants(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-promote-%d", pid))
	owner := addTeamUser(t, db, cust.ID, fmt.Sprintf("owner-promote-%d@x.test", pid), "owner")

	perm, err := db.GetOrgPermission(cust.ID, owner.ID)
	if err != nil {
		t.Fatalf("admin should have a row: %v", err)
	}
	if !perm.CanManageEtc || !perm.CanCreateNamespaces {
		t.Errorf("promotion should materialize both bits; got %+v", perm)
	}
}

// TestTrigger_DemotionRetainsGrants is the case that decided the design.
//
// Demotion leaves the four bits in place. It has to: if it cleared them
// it would need to know which of them predated the promotion, and that
// was never recorded. Because demotion keeps them, a set bit means
// "explicitly granted" in every state, with no provenance to track.
func TestTrigger_DemotionRetainsGrants(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-retain-%d", pid))
	dana := addTeamUser(t, db, cust.ID, fmt.Sprintf("dana-retain-%d@x.test", pid), "owner")

	demote(t, db, cust.ID, dana.ID, "")

	perm, err := db.GetOrgPermission(cust.ID, dana.ID)
	if err != nil {
		t.Fatalf("row should survive demotion: %v", err)
	}
	if !perm.CanManageEtc || !perm.CanCreateNamespaces {
		t.Errorf("demotion must not revoke; got %+v", perm)
	}

	// And the grants are now ordinary member grants, revokable one at a
	// time like anyone else's.
	if err := db.RemoveUserFromGroup(cust.ID, dana.Email, "nsmgr"); err != nil {
		t.Fatalf("deluser after demotion: %v", err)
	}
	perm, _ = db.GetOrgPermission(cust.ID, dana.ID)
	if perm.CanCreateNamespaces {
		t.Error("nsmgr should be revokable after demotion")
	}
	if !perm.CanManageEtc {
		t.Error("revoking nsmgr should leave configmgr alone")
	}
}

// TestTrigger_DemotionNotifiesOperator: the retention is announced, not
// silent. The operator who demoted someone is told what they kept.
func TestTrigger_DemotionNotifiesOperator(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-notify-%d", pid))
	operator := addTeamUser(t, db, cust.ID, fmt.Sprintf("op-notify-%d@x.test", pid), "owner")
	dana := addTeamUser(t, db, cust.ID, fmt.Sprintf("dana-notify-%d@x.test", pid), "owner")

	demote(t, db, cust.ID, dana.ID, operator.ID)

	pending, err := db.PendingNotices(operator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("operator should have one pending notice; got %d", len(pending))
	}
	n := pending[0]
	if n.Key != "member_retained_grants" {
		t.Errorf("notice key = %q", n.Key)
	}
	if got := n.Params["member"]; got != dana.Email {
		t.Errorf("params[member] = %v, want %s", got, dana.Email)
	}
	// The org is named because the notice is addressed to a user, not to
	// one of their memberships: the operator can read it with any org
	// selected in the switcher, including one this has nothing to do with.
	if got := n.Params["org"]; got != cust.Name {
		t.Errorf("params[org] = %v, want %s", got, cust.Name)
	}
	groups, _ := n.Params["groups"].([]any)
	if len(groups) != 2 {
		t.Errorf("params[groups] = %#v, want both retained", n.Params["groups"])
	}

	// The demoted member is not notified — the notice is for whoever has
	// to decide whether to take the grants back.
	danaPending, _ := db.PendingNotices(dana.ID)
	if len(danaPending) != 0 {
		t.Errorf("demoted member should not be notified; got %d", len(danaPending))
	}
}

// TestTrigger_DemotionWithoutActorIsSilent: an out-of-band change has no
// operator to inform. That is a no-op, not an error — the membership
// update must still succeed.
func TestTrigger_DemotionWithoutActorIsSilent(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-silent-%d", pid))
	dana := addTeamUser(t, db, cust.ID, fmt.Sprintf("dana-silent-%d@x.test", pid), "owner")

	demote(t, db, cust.ID, dana.ID, "")

	role, _, _, err := db.MemberGroups(cust.ID, dana.Email)
	if err != nil {
		t.Fatal(err)
	}
	if role != "member" {
		t.Errorf("demotion should have applied; role = %q", role)
	}
	var notices int64
	if err := db.DB().Table("user_notices").Count(&notices).Error; err != nil {
		t.Fatal(err)
	}
	if notices != 0 {
		t.Errorf("no GUC means no notice; got %d rows", notices)
	}
}

// TestTrigger_BogusActorDoesNotBreakDemotion: user_notices.user_id is
// FK'd to users, so a stale or hand-set GUC naming a non-existent user
// would abort the membership update if the trigger inserted blindly.
func TestTrigger_BogusActorDoesNotBreakDemotion(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-bogus-%d", pid))
	dana := addTeamUser(t, db, cust.ID, fmt.Sprintf("dana-bogus-%d@x.test", pid), "owner")

	demote(t, db, cust.ID, dana.ID, "no-such-user-id")

	role, _, _, err := db.MemberGroups(cust.ID, dana.Email)
	if err != nil {
		t.Fatal(err)
	}
	if role != "member" {
		t.Errorf("demotion should have applied despite the bogus actor; role = %q", role)
	}
}

// TestTrigger_RemovalTombstonesGrants: removal is the trigger's job now.
// RemoveMember used to soft-delete the row itself, which made two
// writers for one row and hid the setOrgBit collision for as long as it
// did.
func TestTrigger_RemovalTombstonesGrants(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("trig-remove-%d", pid))
	alice := addTeamUser(t, db, cust.ID, fmt.Sprintf("alice-remove-%d@x.test", pid), "member")
	if err := db.AddUserToGroup(cust.ID, alice.Email, "nsmgr"); err != nil {
		t.Fatal(err)
	}

	if err := db.RemoveMember(cust.ID, alice.Email); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, err := db.GetOrgPermission(cust.ID, alice.ID); err == nil {
		t.Error("removal should tombstone the permission row")
	}
	// The row is tombstoned, not deleted: it still occupies (org, user)
	// in idx_orgperm_org_user, which is what setOrgBit has to cope with.
	var tombstones int64
	err := db.DB().Table("org_permissions").
		Where("org_id = ? AND user_id = ? AND deleted_at IS NOT NULL", cust.ID, alice.ID).
		Count(&tombstones).Error
	if err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 {
		t.Errorf("want 1 tombstoned row, got %d", tombstones)
	}
}
