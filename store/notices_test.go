package store

import (
	"testing"
)

// TestNotices_NotSeenInitially: a fresh user has not seen any notice.
func TestNotices_NotSeenInitially(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-init")
	user, _ := db.CreateUser("alice-init@example.test", cust.ID)

	seen, err := db.HasSeenNotice(user.ID, "tmp_48h_expiry")
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Errorf("fresh user should not have seen notice; got seen=true")
	}
}

// TestNotices_MarkSeenIsSticky: once marked, the flag persists.
func TestNotices_MarkSeenIsSticky(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-sticky")
	user, _ := db.CreateUser("alice-sticky@example.test", cust.ID)

	if err := db.MarkNoticeSeen(user.ID, "tmp_48h_expiry", "warning"); err != nil {
		t.Fatal(err)
	}
	seen, _ := db.HasSeenNotice(user.ID, "tmp_48h_expiry")
	if !seen {
		t.Errorf("after MarkNoticeSeen, HasSeenNotice should return true")
	}
}

// TestNotices_MarkSeenIdempotent: re-marking is safe.
func TestNotices_MarkSeenIdempotent(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-idem")
	user, _ := db.CreateUser("alice-idem@example.test", cust.ID)

	if err := db.MarkNoticeSeen(user.ID, "tmp_48h_expiry", "warning"); err != nil {
		t.Fatal(err)
	}
	// Second mark must not error.
	if err := db.MarkNoticeSeen(user.ID, "tmp_48h_expiry", "warning"); err != nil {
		t.Errorf("second mark errored: %v", err)
	}
}

// TestNotices_PerUser: marking alice's notice doesn't affect bob.
func TestNotices_PerUser(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-peruser")
	alice, _ := db.CreateUser("alice-pu@example.test", cust.ID)
	bob, _ := db.CreateUser("bob-pu@example.test", cust.ID)

	if err := db.MarkNoticeSeen(alice.ID, "tmp_48h_expiry", "warning"); err != nil {
		t.Fatal(err)
	}
	bobSeen, _ := db.HasSeenNotice(bob.ID, "tmp_48h_expiry")
	if bobSeen {
		t.Errorf("marking alice should not affect bob; bobSeen=true")
	}
}

// TestNotices_MultipleKeys: different notice keys are tracked independently.
func TestNotices_MultipleKeys(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-multikey")
	user, _ := db.CreateUser("alice-mk@example.test", cust.ID)

	if err := db.MarkNoticeSeen(user.ID, "key_a", "warning"); err != nil {
		t.Fatal(err)
	}
	if seen, _ := db.HasSeenNotice(user.ID, "key_a"); !seen {
		t.Errorf("key_a should be seen")
	}
	if seen, _ := db.HasSeenNotice(user.ID, "key_b"); seen {
		t.Errorf("key_b should NOT be seen (independent of key_a)")
	}
}

// TestNotices_RejectsInvalidKind: only success/warning/error are valid.
func TestNotices_RejectsInvalidKind(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("notices-bad-kind")
	user, _ := db.CreateUser("alice-bk@example.test", cust.ID)

	err := db.MarkNoticeSeen(user.ID, "key_x", "panic")
	if err == nil {
		t.Errorf("expected error for invalid kind, got nil")
	}
}
