package store

import (
	"testing"
)

// TestNotices_NonePendingInitially: a fresh user has nothing queued.
func TestNotices_NonePendingInitially(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-init@example.test")

	pending, err := db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("fresh user should have no pending notices; got %d", len(pending))
	}
}

// TestNotices_PendingUntilDrained: emitting queues a notice that stays
// pending across reads. This is the 071 regression that would otherwise
// be invisible — under the old suppression ledger a row's mere existence
// meant "already shown", so an enqueued-but-undelivered notice would
// suppress itself and never render.
func TestNotices_PendingUntilDrained(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-pending@example.test")

	if err := db.EmitNotice(user.ID, "tmp_48h_expiry", "warning", nil); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		pending, err := db.PendingNotices(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 {
			t.Fatalf("read %d: want 1 pending notice, got %d", i, len(pending))
		}
		if pending[0].Key != "tmp_48h_expiry" || pending[0].Kind != "warning" {
			t.Fatalf("read %d: got %+v", i, pending[0])
		}
	}
}

// TestNotices_StackAndDrainOnce: two emissions of the same key leave two
// pending rows — there is no uniqueness and no per-key suppression. One
// drain returns both and leaves nothing; a second drain returns nothing.
func TestNotices_StackAndDrainOnce(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-stack@example.test")

	for range 2 {
		if err := db.EmitNotice(user.ID, "tmp_48h_expiry", "warning", nil); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("two emissions should leave two rows; got %d", len(pending))
	}

	ids := []int64{pending[0].ID, pending[1].ID}
	if ids[0] == ids[1] {
		t.Fatalf("instances share an id: %v", ids)
	}
	if err := db.MarkNoticesSeen(ids); err != nil {
		t.Fatal(err)
	}

	pending, err = db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("second drain should return nothing; got %d", len(pending))
	}
}

// TestNotices_ParamsRoundTrip: params carries the facts a renderer
// interpolates. The registry owns the prose; the row owns the values.
func TestNotices_ParamsRoundTrip(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-params@example.test")

	err := db.EmitNotice(user.ID, "member_retained_grants", "warning", map[string]any{
		"member": "dana@x.test",
		"groups": []string{"binmgr", "configmgr"},
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("want 1 pending notice, got %d", len(pending))
	}
	if got := pending[0].Params["member"]; got != "dana@x.test" {
		t.Errorf("params[member] = %v, want dana@x.test", got)
	}
	groups, ok := pending[0].Params["groups"].([]any)
	if !ok || len(groups) != 2 || groups[0] != "binmgr" || groups[1] != "configmgr" {
		t.Errorf("params[groups] = %#v, want [binmgr configmgr]", pending[0].Params["groups"])
	}
}

// TestNotices_PerUser: alice's queue is not bob's.
func TestNotices_PerUser(t *testing.T) {
	db := testOpen(t)
	alice, _ := db.CreateUser("alice-pu@example.test")
	bob, _ := db.CreateUser("bob-pu@example.test")

	if err := db.EmitNotice(alice.ID, "tmp_48h_expiry", "warning", nil); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingNotices(bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("emitting to alice should not queue for bob; got %d", len(pending))
	}
}

// TestNotices_MarkSeenIsScopedToIDs: draining one instance leaves the
// other pending, even under the same key.
func TestNotices_MarkSeenIsScopedToIDs(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-scoped@example.test")

	for range 2 {
		if err := db.EmitNotice(user.ID, "tmp_48h_expiry", "warning", nil); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := db.PendingNotices(user.ID)
	if len(pending) != 2 {
		t.Fatalf("setup: want 2 pending, got %d", len(pending))
	}

	if err := db.MarkNoticesSeen([]int64{pending[0].ID}); err != nil {
		t.Fatal(err)
	}
	after, err := db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != pending[1].ID {
		t.Errorf("want only id %d still pending; got %+v", pending[1].ID, after)
	}
}

// TestNotices_MarkSeenEmptyIsNoop: draining nothing must not error or
// mark everything. Drain() calls this on every quiet request.
func TestNotices_MarkSeenEmptyIsNoop(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-empty@example.test")

	if err := db.EmitNotice(user.ID, "tmp_48h_expiry", "warning", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkNoticesSeen(nil); err != nil {
		t.Errorf("MarkNoticesSeen(nil) errored: %v", err)
	}
	pending, _ := db.PendingNotices(user.ID)
	if len(pending) != 1 {
		t.Errorf("empty mark should leave the queue untouched; got %d pending", len(pending))
	}
}

// TestNotices_RejectsInvalidKind: only success/warning/error are valid.
// The check catches registry/db drift before it becomes a constraint
// violation from inside a trigger.
func TestNotices_RejectsInvalidKind(t *testing.T) {
	db := testOpen(t)
	user, _ := db.CreateUser("alice-bk@example.test")

	if err := db.EmitNotice(user.ID, "key_x", "panic", nil); err == nil {
		t.Errorf("expected error for invalid kind, got nil")
	}
}
