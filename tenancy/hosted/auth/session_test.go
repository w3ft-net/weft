package auth

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"

	"github.com/graybeardcorp/saaskit/orgs"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() {
		db.TruncateTestTables()
		db.Close()
	})
	return db
}

// addMember inserts (or updates) a memberships row with the given
// role, returning the user.
func addMember(t *testing.T, db *store.Store, custID store.OrgID, role string) *store.User {
	t.Helper()
	user, err := db.CreateUser(fmt.Sprintf("user-%s-%d@example.com", role, time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	err = db.AddTeamMemberWithEmail(custID, user.ID, user.Email, testRole(role))
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// A session says whether the member owns the org; what they may write is the
// Authorizer's answer, from their org_permissions row.
func TestBuildSession_IsOwnerFollowsRole(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-role-%d", os.Getpid()))
	owner := addMember(t, db, cust.ID, "owner")
	member := addMember(t, db, cust.ID, "member")

	for user, want := range map[string]bool{owner.ID: true, member.ID: false} {
		sess, err := BuildSession(db, user, "")
		if err != nil {
			t.Fatal(err)
		}
		if sess.IsOwner != want {
			t.Errorf("IsOwner = %v, want %v", sess.IsOwner, want)
		}
	}
}

// TestBuildSession_AdminMembershipMaterializesRow: the trigger, seen
// from the Go side. Adding an admin membership creates the row that
// BuildSession will read.
func TestBuildSession_AdminMembershipMaterializesRow(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-materialize-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "owner")

	perm, err := db.GetOrgPermission(cust.ID, user.ID)
	if err != nil {
		t.Fatalf("admin should have an org_permissions row: %v", err)
	}
	if !perm.CanManageEtc || !perm.CanCreateNamespaces {
		t.Errorf("materialized row should hold both bits; got %+v", perm)
	}
}

// testRole maps the fixtures' historical "owner"/"member" words onto the
// shared orgs vocabulary weft adopted, so the table-driven cases keep reading
// the way the product talks about roles.
func testRole(role string) orgs.Role {
	if role == "owner" {
		return store.RoleOwner
	}
	return orgs.RoleMember
}
