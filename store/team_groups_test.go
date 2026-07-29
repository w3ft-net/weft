package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// groupsFor is a test helper: the sorted group tokens for a member.
func groupsFor(t *testing.T, db *Store, custID string, email string) []string {
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
	// Owners can't be granted explicit groups.
	if err := db.AddUserToGroup(cust.ID, owner, "cronmgr"); err == nil || !strings.Contains(err.Error(), "implicit full access") {
		t.Fatalf("expected owner error, got %v", err)
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
	for _, want := range []string{"USER", "ROLE", "GROUPS", owner, "(all — implicit)", alice, "payments"} {
		if !strings.Contains(out, want) {
			t.Errorf("passwd table missing %q:\n%s", want, out)
		}
	}
}

func mustMember(t *testing.T, db *Store, custID string, email string) *TeamMember {
	t.Helper()
	m, err := db.GetTeamMemberByEmail(custID, email)
	if err != nil {
		t.Fatalf("GetTeamMemberByEmail(%s): %v", email, err)
	}
	return m
}
