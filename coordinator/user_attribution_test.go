package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/notices"
	"github.com/graybeardcorp/weft/store"
)

// TestCoordinatorQuery_RecordsCreatorOnTmpWrite is the end-to-end
// integration that proves user_id flows from Coordinator.Query
// through runLocal → ShellFS → store.WriteTmpFile and lands in the
// row's created_by_user_id column. If anything in that chain drops
// the userID, the test fails.
func TestCoordinatorQuery_RecordsCreatorOnTmpWrite(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	user, err := db.CreateUser("alice-coord@example.test")
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	// Use the customer's name (not its ID) — Query takes a name.
	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Execute(context.Background(), `echo coord-test > /tmp/note`, tr, cust.Name, user.ID)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	drainExec(t, res)

	row, err := db.GetTmpFile(custID, "/tmp/note")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.CreatedByUserID == nil || *row.CreatedByUserID != user.ID {
		t.Errorf("CreatedByUserID after Coordinator.Query: got %v, want %q",
			row.CreatedByUserID, user.ID)
	}
}

// TestCoordinatorQuery_EmptyUserStoresNullCreator covers the
// system-invocation path: Query without a userID (cron, automated
// tooling, the dev shell) should still succeed and store NULL on the
// row.
func TestCoordinatorQuery_EmptyUserStoresNullCreator(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	cust, _ := db.GetOrgByID(custID)
	res, err := c.Execute(context.Background(), `echo system > /tmp/auto`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	drainExec(t, res)

	row, err := db.GetTmpFile(custID, "/tmp/auto")
	if err != nil {
		t.Fatal(err)
	}
	if row.CreatedByUserID != nil {
		t.Errorf("empty userID should store NULL; got %q", *row.CreatedByUserID)
	}
}

// TestCoordinatorQuery_AttachesTmpExpiryNoticeOnFirstWrite verifies
// the end-to-end notice path: the first /tmp write under a userID
// produces a Notice on the response. Second call from same user
// produces no notice. Different user gets their own notice.
func TestCoordinatorQuery_AttachesTmpExpiryNoticeOnFirstWrite(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	alice, _ := db.CreateUser("alice-notice@example.test")
	bob, _ := db.CreateUser("bob-notice@example.test")

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := db.GetOrgByID(custID)

	// Alice's first /tmp write — should get the notice.
	r1, err := c.Execute(context.Background(), `echo first > /tmp/a`, tr, cust.Name, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	r1D := drainExec(t, r1)
	if len(r1D.Notices) != 1 {
		t.Fatalf("alice's first write: got %d notices, want 1", len(r1D.Notices))
	}
	if r1D.Notices[0].Key != "tmp_48h_expiry" {
		t.Errorf("notice key: got %q, want tmp_48h_expiry", r1D.Notices[0].Key)
	}
	if r1D.Notices[0].Kind != notices.KindWarning {
		t.Errorf("notice kind: got %q, want warning", r1D.Notices[0].Kind)
	}

	// Alice's second write — should NOT get the notice.
	r2, err := c.Execute(context.Background(), `echo second >> /tmp/a`, tr, cust.Name, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	r2D := drainExec(t, r2)
	if len(r2D.Notices) != 0 {
		t.Errorf("alice's second write: got %d notices, want 0", len(r2D.Notices))
	}

	// Bob's first write — should get HIS first notice (independent of alice).
	r3, err := c.Execute(context.Background(), `echo bobs > /tmp/b`, tr, cust.Name, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	r3D := drainExec(t, r3)
	if len(r3D.Notices) != 1 {
		t.Errorf("bob's first write: got %d notices, want 1 (per-user, not per-customer)", len(r3D.Notices))
	}
}

// TestCoordinatorExecuteForWatch_StoresNullCreator: watches run as the
// system, not as a user. Any /tmp write a watch performs (rare, but
// possible — `cat /log/x | filter > /tmp/scratch` from inside a
// watch body) lands with NULL creator.
func TestCoordinatorExecuteForWatch_StoresNullCreator(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveCronWatch(custID, "", "scratchwrite", "every 1m", `echo from-watch > /tmp/watchnote`, true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "scratchwrite")
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := c.ExecuteForWatch(context.Background(), `echo from-watch > /tmp/watchnote`, tr, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	drainExec(t, res)

	row, err := db.GetTmpFile(custID, "/tmp/watchnote")
	if err != nil {
		t.Fatal(err)
	}
	if row.CreatedByUserID != nil {
		t.Errorf("watch write should be unattributed; got creator %q", *row.CreatedByUserID)
	}
}

// TestCoordinatorExecute_RedirectUsesMemberGroups: a redirect into /bin
// runs with the caller's org_permissions row, the same authority vi's
// /api/fs/write save uses. A binmgr member's write lands; a member without
// the group, and the system identity crons run as, are denied.
func TestCoordinatorExecute_RedirectUsesMemberGroups(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	alice, err := db.CreateUser("alice-redirect@example.test")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.CreateUser("bob-redirect@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DB().WithContext(store.WithOrg(t.Context(), custID)).Create(&store.OrgPermission{
		OrgID: custID, UserID: alice.ID, CanManageFunctions: true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := db.GetOrgByID(custID)
	write := func(userID, name string) string {
		res, err := c.Execute(context.Background(), "echo 'echo hi' > /bin/"+name, tr, cust.Name, userID)
		if err != nil {
			t.Fatalf("Execute as %q: %v", userID, err)
		}
		return drainExec(t, res).Stderr
	}

	if stderr := write(alice.ID, "from-alice"); stderr != "" {
		t.Errorf("binmgr member's redirect: stderr %q", stderr)
	}
	if fn, err := db.GetShellFunction(custID, "from-alice"); err != nil || fn == nil {
		t.Errorf("binmgr member's redirect didn't save /bin/from-alice: %v", err)
	}

	for who, userID := range map[string]string{"member without binmgr": bob.ID, "system": ""} {
		write(userID, "denied")
		if fn, err := db.GetShellFunction(custID, "denied"); err == nil && fn != nil {
			t.Errorf("%s's redirect saved /bin/denied", who)
		}
	}
}
