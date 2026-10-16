package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// NamespaceContents reports what keeps a namespace from being removed:
// its sources and its live (unrevoked) tokens.
func TestNamespaceContents(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("ns-contents")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "doomed")
	if err != nil {
		t.Fatal(err)
	}

	c, err := db.NamespaceContents(cust.ID, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sources) != 0 || c.Tokens != 0 {
		t.Fatalf("empty namespace reports %+v", c)
	}

	if err := db.UpsertPendingLogGroup(ns.ID, "b", "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPendingLogGroup(ns.ID, "a", "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateAgentToken(cust.ID, ns.ID, "live", ""); err != nil {
		t.Fatal(err)
	}
	_, revoked, err := db.CreateAgentToken(cust.ID, ns.ID, "dead", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.scoped(cust.ID).Model(revoked).Update("revoked", true).Error; err != nil {
		t.Fatal(err)
	}

	c, err = db.NamespaceContents(cust.ID, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sources) != 2 || c.Sources[0] != "a" || c.Sources[1] != "b" {
		t.Errorf("sources = %v, want [a b]", c.Sources)
	}
	if c.Tokens != 1 {
		t.Errorf("tokens = %d, want 1 (the revoked one does not count)", c.Tokens)
	}
}

// DeleteNamespace removes the namespace for good: its sources, tokens and
// permissions go with it, and the name is free to be created again. A soft
// delete would keep the (org, name) unique row and leave the children behind.
func TestDeleteNamespaceIsHardAndFreesTheName(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("ns-delete")
	if err != nil {
		t.Fatal(err)
	}
	ns, err := db.FindOrCreateNamespace(cust.ID, "doomed")
	if err != nil {
		t.Fatal(err)
	}
	keep, err := db.FindOrCreateNamespace(cust.ID, "keeper")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []uint{ns.ID, keep.ID} {
		if err := db.UpsertPendingLogGroup(n, "svc", "h", "d", `["x"]`, "{}", 1, false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.CreateAgentToken(cust.ID, n, "t", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertNamespacePermission(&NamespacePermission{UserID: "u1", NamespaceID: n, CanRead: true}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.DeleteNamespace(cust.ID, "doomed"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	if _, err := db.GetNamespace(cust.ID, "doomed"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("GetNamespace after delete = %v, want not found", err)
	}
	count := func(table string, nsID uint) (n int64) {
		t.Helper()
		if err := db.DB().Unscoped().Table(table).Where("namespace_id = ?", nsID).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"log_groups", "agent_tokens", "namespace_permissions"} {
		if n := count(table, ns.ID); n != 0 {
			t.Errorf("%s still has %d rows for the deleted namespace", table, n)
		}
		if n := count(table, keep.ID); n != 1 {
			t.Errorf("%s has %d rows for the other namespace, want 1 untouched", table, n)
		}
	}

	again, err := db.FindOrCreateNamespace(cust.ID, "doomed")
	if err != nil || again.ID == ns.ID {
		t.Errorf("recreating the name: id=%d err=%v, want a fresh namespace", again.ID, err)
	}

	if err := db.DeleteNamespace(cust.ID, "nope"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("deleting a missing namespace = %v, want not found", err)
	}
}
