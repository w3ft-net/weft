package auth

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevlarcon/weft/store"
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

// addMember inserts (or updates) a team_members row with the given
// role, returning the user.
func addMember(t *testing.T, db *store.Store, custID uint, role string) *store.User {
	t.Helper()
	user, err := db.CreateUser(fmt.Sprintf("user-%s-%d@example.com", role, time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = db.CreateTeamMember(&store.TeamMember{
		ID:         uuid.New().String(),
		CustomerID: custID,
		Email:      user.Email,
		UserID:     &user.ID,
		Role:       role,
		Status:     "active",
		InvitedAt:  now,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestBuildSession_OwnerGetsAllFlags(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("sess-owner-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "owner")

	sess, err := BuildSession(db, user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.IsOwner {
		t.Error("expected IsOwner=true")
	}
	if !sess.CanManageFunctions || !sess.CanManageWatches || !sess.CanManageEtc {
		t.Errorf("owner should bypass to all flags; got bin=%v cron=%v etc=%v",
			sess.CanManageFunctions, sess.CanManageWatches, sess.CanManageEtc)
	}
}

func TestBuildSession_MemberWithoutPermissionRowIsReadOnly(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("sess-member-%d", os.Getpid()))
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
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("sess-flags-%d", os.Getpid()))
	user := addMember(t, db, cust.ID, "member")

	err := db.ExecForTest(
		`INSERT INTO org_permissions (customer_id, user_id, can_manage_functions, can_manage_watches, can_manage_etc) VALUES (?, ?, true, false, true)`,
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
