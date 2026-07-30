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
func addMember(t *testing.T, db *store.Store, custID string, role string) *store.User {
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

// TestBuildSession_OwnerGetsAllFlags: an owner has every flag. Note the
// mechanism, which changed in migration 072: the flags are true because
// becoming an active admin materializes an all-bits-true org_permissions
// row, not because BuildSession bypasses the row for owners.
func TestBuildSession_OwnerGetsAllFlags(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-owner-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "owner")

	sess, err := BuildSession(db, user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.IsOwner {
		t.Error("expected IsOwner=true")
	}
	if !sess.CanManageFunctions || !sess.CanManageWatches || !sess.CanManageEtc || !sess.CanCreateNamespaces {
		t.Errorf("owner should hold all four bits; got bin=%v cron=%v etc=%v ns=%v",
			sess.CanManageFunctions, sess.CanManageWatches, sess.CanManageEtc, sess.CanCreateNamespaces)
	}
}

// TestBuildSession_OwnerReadsBitsFromRow: role is no longer a permission
// authority. An owner whose org_permissions row has been cleared reads
// as having none of the four bits — proof that BuildSession consults the
// row and nothing else.
//
// This is what "org_permissions is authoritative" means in practice, and
// it is the case that used to be impossible to express: before migration
// 072 an owner's bits were recomputed from the role on every request, so
// clearing the row changed nothing.
func TestBuildSession_OwnerReadsBitsFromRow(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-owner-cleared-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "owner")

	err := db.ExecForTest(
		`UPDATE org_permissions
		    SET can_manage_functions = false, can_manage_watches = false,
		        can_manage_etc = false, can_create_namespaces = false
		  WHERE org_id = ? AND user_id = ?`,
		cust.ID, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := BuildSession(db, user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.IsOwner {
		t.Error("role is unchanged, so IsOwner should still be true")
	}
	if sess.CanManageFunctions || sess.CanManageWatches || sess.CanManageEtc || sess.CanCreateNamespaces {
		t.Errorf("cleared row should read as no bits; got bin=%v cron=%v etc=%v ns=%v",
			sess.CanManageFunctions, sess.CanManageWatches, sess.CanManageEtc, sess.CanCreateNamespaces)
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
	if !perm.CanManageFunctions || !perm.CanManageWatches || !perm.CanManageEtc || !perm.CanCreateNamespaces {
		t.Errorf("materialized row should hold all four bits; got %+v", perm)
	}
}

func TestBuildSession_MemberWithoutPermissionRowIsReadOnly(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-member-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "member")

	sess, err := BuildSession(db, user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.IsOwner {
		t.Error("expected IsOwner=false for member")
	}
	if sess.CanManageFunctions || sess.CanManageWatches || sess.CanManageEtc {
		t.Errorf("member with no perm row should be all-false; got bin=%v cron=%v etc=%v",
			sess.CanManageFunctions, sess.CanManageWatches, sess.CanManageEtc)
	}
}

func TestBuildSession_MemberWithFlagsHasThem(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("sess-flags-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "member")

	err := db.ExecForTest(
		`INSERT INTO org_permissions (org_id, user_id, can_manage_functions, can_manage_watches, can_manage_etc) VALUES (?, ?, true, false, true)`,
		cust.ID, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	sess, err := BuildSession(db, user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.IsOwner {
		t.Error("expected IsOwner=false")
	}
	if !sess.CanManageFunctions || !sess.CanManageEtc {
		t.Error("expected the upserted flags to round-trip")
	}
	if sess.CanManageWatches {
		t.Error("expected CanManageWatches to remain false")
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
