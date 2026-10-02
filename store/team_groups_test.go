package store

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
)

// groupsFor is a test helper: the sorted group tokens for a member.
func groupsFor(t *testing.T, db *Store, custID OrgID, email string) []string {
	t.Helper()
	_, _, groups, err := db.MemberGroups(custID, email)
	if err != nil {
		t.Fatalf("MemberGroups(%s): %v", email, err)
	}
	return groups
}

func TestTeamGroups_GrantRevoke(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tg-%d", pid))
	owner := fmt.Sprintf("owner-%d@x.test", pid)
	alice := fmt.Sprintf("alice-%d@x.test", pid)
	_ = addTeamUser(t, db, cust.ID, owner, "owner")
	_ = addTeamUser(t, db, cust.ID, alice, "member")
	if _, err := db.FindOrCreateNamespace(cust.ID, "payments"); err != nil {
		t.Fatal(err)
	}

	// Manager group → OrgPermission bit.
	if err := db.AddUserToGroup(cust.ID, alice, "cronmgr"); err != nil {
		t.Fatalf("add cronmgr: %v", err)
	}
	// Namespace read + tokens are independent facets of one ns group.
	if err := db.AddUserToGroup(cust.ID, alice, "payments"); err != nil {
		t.Fatalf("add payments: %v", err)
	}
	if err := db.AddUserToGroup(cust.ID, alice, "payments+tokens"); err != nil {
		t.Fatalf("add payments+tokens: %v", err)
	}

	got := groupsFor(t, db, cust.ID, alice)
	want := []string{"cronmgr", "payments", "payments+tokens"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("groups after grant: got %v want %v", got, want)
	}

	// Revoking the read facet leaves the tokens facet intact.
	if err := db.RemoveUserFromGroup(cust.ID, alice, "payments"); err != nil {
		t.Fatalf("remove payments: %v", err)
	}
	got = groupsFor(t, db, cust.ID, alice)
	want = []string{"cronmgr", "payments+tokens"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("groups after revoke read: got %v want %v", got, want)
	}

	// Revoking the last namespace facet drops the row entirely.
	if err := db.RemoveUserFromGroup(cust.ID, alice, "payments+tokens"); err != nil {
		t.Fatalf("remove payments+tokens: %v", err)
	}
	if perms, _ := db.ListUserNamespacePermissions(mustMember(t, db, cust.ID, alice).UserID); len(perms) != 0 {
		t.Fatalf("expected no namespace perm rows, got %d", len(perms))
	}

	// Revoking the manager group clears the org bit.
	if err := db.RemoveUserFromGroup(cust.ID, alice, "cronmgr"); err != nil {
		t.Fatalf("remove cronmgr: %v", err)
	}
	if got := groupsFor(t, db, cust.ID, alice); len(got) != 0 {
		t.Fatalf("expected no groups after full revoke, got %v", got)
	}
}

func TestTeamGroups_Errors(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tge-%d", pid))
	owner := fmt.Sprintf("owner-%d@x.test", pid)
	alice := fmt.Sprintf("alice-%d@x.test", pid)
	_ = addTeamUser(t, db, cust.ID, owner, "owner")
	_ = addTeamUser(t, db, cust.ID, alice, "member")

	// Unknown namespace/group.
	if err := db.AddUserToGroup(cust.ID, alice, "nope"); err == nil || !strings.Contains(err.Error(), "no such group or namespace") {
		t.Fatalf("expected unknown-group error, got %v", err)
	}
	// Owners are grantable and revokable like anyone else. This used to
	// be an error ("owners have implicit full access") because there was
	// nothing to grant to; since migration 072 an owner holds the same
	// four bits, so `deluser <owner> binmgr` has something to do.
	if err := db.RemoveUserFromGroup(cust.ID, owner, "binmgr"); err != nil {
		t.Fatalf("deluser owner binmgr: %v", err)
	}
	if got := groupsFor(t, db, cust.ID, owner); slices.Contains(got, "binmgr") {
		t.Errorf("binmgr should be revoked from the owner; groups = %v", got)
	}
	if err := db.AddUserToGroup(cust.ID, owner, "binmgr"); err != nil {
		t.Fatalf("adduser owner binmgr: %v", err)
	}
	if got := groupsFor(t, db, cust.ID, owner); !slices.Contains(got, "binmgr") {
		t.Errorf("binmgr should be restored to the owner; groups = %v", got)
	}
	// Non-member.
	if err := db.AddUserToGroup(cust.ID, "ghost@x.test", "cronmgr"); err == nil || !strings.Contains(err.Error(), "no such member") {
		t.Fatalf("expected no-such-member error, got %v", err)
	}
}

func TestTeamGroups_IsOwnerAndRemoveMember(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tgo-%d", pid))
	ownerU := addTeamUser(t, db, cust.ID, fmt.Sprintf("owner-%d@x.test", pid), "owner")
	aliceU := addTeamUser(t, db, cust.ID, fmt.Sprintf("alice-%d@x.test", pid), "member")

	if ok, _ := db.IsOwner(cust.ID, ownerU.ID); !ok {
		t.Error("owner should be owner")
	}
	if ok, _ := db.IsOwner(cust.ID, aliceU.ID); ok {
		t.Error("member should not be owner")
	}

	// Owners can't be removed; members can.
	if err := db.RemoveMember(cust.ID, ownerU.Email); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("expected owner-removal rejection, got %v", err)
	}
	if err := db.RemoveMember(cust.ID, aliceU.Email); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if _, err := db.GetTeamMemberByEmail(cust.ID, aliceU.Email); err == nil {
		t.Error("removed member should not be found")
	}
}

func TestTeamGroups_RenderPasswd(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tgp-%d", pid))
	owner := fmt.Sprintf("owner-%d@x.test", pid)
	alice := fmt.Sprintf("alice-%d@x.test", pid)
	_ = addTeamUser(t, db, cust.ID, owner, "owner")
	_ = addTeamUser(t, db, cust.ID, alice, "member")
	if _, err := db.FindOrCreateNamespace(cust.ID, "payments"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUserToGroup(cust.ID, alice, "payments"); err != nil {
		t.Fatal(err)
	}

	out, err := db.RenderPasswd(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The owner's groups render as real tokens. There is no
	// "(all — implicit)" special case any more: an owner's access is
	// four rows like everyone else's, so every rendering site reads it
	// the same way.
	for _, want := range []string{"USER", "ROLE", "GROUPS", owner, "binmgr", "cronmgr", "configmgr", "nsmgr", alice, "payments"} {
		if !strings.Contains(out, want) {
			t.Errorf("passwd table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "implicit") {
		t.Errorf("passwd table should have no implicit-access special case:\n%s", out)
	}
}

// TestTeamGroups_GrantAfterRemoveAndReadd: granting a group to a member
// who was removed and re-added must work.
//
// It didn't. Removal soft-deletes the org_permissions row, the lookup in
// setOrgBit is soft-delete-scoped so it misses the tombstone, and the
// fallthrough Create then collides with idx_orgperm_org_user — which has
// no deleted_at predicate and so still sees the tombstone. Migration 072
// hands removal to a trigger, which makes tombstones the normal case
// rather than an edge one.
//
// Reviving the tombstone must not restore the bits it carried: re-adding
// a member restores the membership, not the grants.
func TestTeamGroups_GrantAfterRemoveAndReadd(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("tg-readd-%d", pid))
	alice := fmt.Sprintf("alice-readd-%d@x.test", pid)
	u := addTeamUser(t, db, cust.ID, alice, "member")

	if err := db.AddUserToGroup(cust.ID, alice, "binmgr"); err != nil {
		t.Fatalf("initial grant: %v", err)
	}
	if err := db.RemoveMember(cust.ID, alice); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := db.AddTeamMemberWithEmail(cust.ID, u.ID, alice, orgs.RoleMember); err != nil {
		t.Fatalf("re-add: %v", err)
	}

	if err := db.AddUserToGroup(cust.ID, alice, "cronmgr"); err != nil {
		t.Fatalf("grant after re-add: %v", err)
	}

	got := groupsFor(t, db, cust.ID, alice)
	want := []string{"cronmgr"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("groups after re-add: got %v want %v (the tombstone's old binmgr must not come back)", got, want)
	}
}

func mustMember(t *testing.T, db *Store, custID OrgID, email string) *TeamMember {
	t.Helper()
	m, err := db.GetTeamMemberByEmail(custID, email)
	if err != nil {
		t.Fatalf("GetTeamMemberByEmail(%s): %v", email, err)
	}
	return m
}
