package mounts

import (
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"

	"github.com/graybeardcorp/weft/store"
)

// Checking whether a member may read /log/<ns> must never create <ns>.
// canReadNamespace used FindOrCreateNamespace, so any non-owner who listed
// or stat'd /log/<anything> left a new namespace behind (and then got
// "permission denied", because a fresh namespace has no permission row).
func TestCanReadNamespace_NeverCreatesNamespaces(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	org, err := tx.FindOrCreateOrg("mounts-perm-test")
	if err != nil {
		t.Fatal(err)
	}
	user, err := tx.CreateVerifiedUser("mounts-perm@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tx.AddTeamMemberWithEmail(org.ID, user.ID, user.Email, orgs.RoleMember)

	readable, err := tx.FindOrCreateNamespace(org.ID, "readable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.FindOrCreateNamespace(org.ID, "private"); err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: user.ID, NamespaceID: readable.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}

	m := &LogMount{cfg: Config{Store: tx, OrgID: org.ID, UserID: user.ID}}

	if !m.canReadNamespace("readable") {
		t.Error("a member with read permission must read their namespace")
	}
	if m.canReadNamespace("private") {
		t.Error("a member without a permission row must not read the namespace")
	}
	for _, typo := range []string{"typo", "readabel", "no such"} {
		if m.canReadNamespace(typo) {
			t.Errorf("%q does not exist and must not be readable", typo)
		}
		if _, err := m.ReadDir(typo); err == nil {
			t.Errorf("ReadDir(%q) on a missing namespace should fail", typo)
		}
		if _, err := tx.GetNamespace(org.ID, typo); err == nil {
			t.Errorf("probing /log/%s created a namespace", typo)
		}
	}
}
