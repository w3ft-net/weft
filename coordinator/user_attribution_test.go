package coordinator

import (
	"testing"
	"time"
)

// TestCoordinatorQuery_RecordsCreatorOnTmpWrite is the end-to-end
// integration that proves user_id flows from Coordinator.Query
// through runLocal → ShellFS → store.WriteTmpFile and lands in the
// row's created_by_user_id column. If anything in that chain drops
// the userID, the test fails.
func TestCoordinatorQuery_RecordsCreatorOnTmpWrite(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	user, err := db.CreateUser("alice-coord@example.test", custID)
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	// Use the customer's name (not its ID) — Query takes a name.
	cust, err := db.GetCustomerByID(custID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Execute(`echo coord-test > /tmp/note`, tr, cust.Name, user.ID); err != nil {
		t.Fatalf("Query: %v", err)
	}

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

	c := NewCoordinator(CoordinatorConfig{Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	cust, _ := db.GetCustomerByID(custID)
	if _, err := c.Execute(`echo system > /tmp/auto`, tr, cust.Name, ""); err != nil {
		t.Fatalf("Query: %v", err)
	}

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
	alice, _ := db.CreateUser("alice-notice@example.test", custID)
	bob, _ := db.CreateUser("bob-notice@example.test", custID)

	c := NewCoordinator(CoordinatorConfig{Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := db.GetCustomerByID(custID)

	// Alice's first /tmp write — should get the notice.
	r1, err := c.Execute(`echo first > /tmp/a`, tr, cust.Name, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Notices) != 1 {
		t.Fatalf("alice's first write: got %d notices, want 1", len(r1.Notices))
	}
	if r1.Notices[0].Key != "tmp_48h_expiry" {
		t.Errorf("notice key: got %q, want tmp_48h_expiry", r1.Notices[0].Key)
	}
	if r1.Notices[0].Kind != NoticeKindWarning {
		t.Errorf("notice kind: got %q, want warning", r1.Notices[0].Kind)
	}

	// Alice's second write — should NOT get the notice.
	r2, err := c.Execute(`echo second >> /tmp/a`, tr, cust.Name, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Notices) != 0 {
		t.Errorf("alice's second write: got %d notices, want 0", len(r2.Notices))
	}

	// Bob's first write — should get HIS first notice (independent of alice).
	r3, err := c.Execute(`echo bobs > /tmp/b`, tr, cust.Name, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r3.Notices) != 1 {
		t.Errorf("bob's first write: got %d notices, want 1 (per-user, not per-customer)", len(r3.Notices))
	}
}

// TestCoordinatorExecuteForWatch_StoresNullCreator: watches run as the
// system, not as a user. Any /tmp write a watch performs (rare, but
// possible — `cat /log/x | filter > /tmp/scratch` from inside a
// watch body) lands with NULL creator.
func TestCoordinatorExecuteForWatch_StoresNullCreator(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveCronWatch(custID, "", "scratchwrite", "every 1m",
		`echo from-watch > /tmp/watchnote`); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, "scratchwrite")
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	if _, err := c.ExecuteForWatch(`echo from-watch > /tmp/watchnote`, tr, w); err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}

	row, err := db.GetTmpFile(custID, "/tmp/watchnote")
	if err != nil {
		t.Fatal(err)
	}
	if row.CreatedByUserID != nil {
		t.Errorf("watch write should be unattributed; got creator %q", *row.CreatedByUserID)
	}
}
