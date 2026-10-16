package notices

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// testStore opens a transaction-scoped Store rolled back at the end of
// the test. Notices are per-user rows, so an org has to exist for
// CreateUser to hang users off, but no test here cares which one.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})
	if _, err := tx.FindOrCreateOrg("notices-test"); err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestNoticeQueue_FirstSignalQueues: a signaled notice drains once.
func TestNoticeQueue_FirstSignalQueues(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-nq1@example.test")

	q := NewQueue(db, user.ID)
	q.Maybe(context.Background(), "tmp_48h_expiry")

	got := q.Drain(context.Background())
	if len(got) != 1 {
		t.Fatalf("first drain: got %d notices, want 1", len(got))
	}
	if got[0].Key != "tmp_48h_expiry" {
		t.Errorf("notice key: got %q, want tmp_48h_expiry", got[0].Key)
	}
	if got[0].Kind != KindWarning {
		t.Errorf("notice kind: got %q, want warning", got[0].Kind)
	}
}

// TestNoticeQueue_MaybeDedupsWithinRequest: signaling the same key
// repeatedly within one request queues at most once. A pipeline forks a
// goroutine per stage, so without this a single command could write
// hundreds of identical rows.
func TestNoticeQueue_MaybeDedupsWithinRequest(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-nq2@example.test")

	q := NewQueue(db, user.ID)
	q.Maybe(context.Background(), "tmp_48h_expiry")
	q.Maybe(context.Background(), "tmp_48h_expiry")
	q.Maybe(context.Background(), "tmp_48h_expiry")

	if got := len(q.Drain(context.Background())); got != 1 {
		t.Errorf("multiple Maybe → got %d queued, want 1", got)
	}
}

// TestNoticeQueue_DrainDeliversOnce: a drained notice does not come back
// on the next request, but a *fresh signal* does. The queue no longer
// suppresses by key across requests — that is emitter policy now — so an
// emitter that fires on every call produces a notice on every request.
func TestNoticeQueue_DrainDeliversOnce(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-nq3@example.test")

	q1 := NewQueue(db, user.ID)
	q1.Maybe(context.Background(), "tmp_48h_expiry")
	if got := len(q1.Drain(context.Background())); got != 1 {
		t.Fatalf("first drain: got %d, want 1", got)
	}

	// Nothing new signaled: the delivered row stays delivered.
	if got := len(NewQueue(db, user.ID).Drain(context.Background())); got != 0 {
		t.Errorf("drain with nothing signaled: got %d, want 0", got)
	}

	// Signaled again: delivered again.
	q2 := NewQueue(db, user.ID)
	q2.Maybe(context.Background(), "tmp_48h_expiry")
	if got := len(q2.Drain(context.Background())); got != 1 {
		t.Errorf("re-signal in a new request: got %d, want 1", got)
	}
}

// TestNoticeQueue_DrainsOutOfBandNotices: the whole point of the queue.
// A notice emitted with no request in flight — as migration 072's
// membership trigger does — reaches the user on their next command,
// without the emitter knowing anything about delivery.
func TestNoticeQueue_DrainsOutOfBandNotices(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-oob@example.test")

	err := db.EmitNotice(user.ID, KeyMemberRetainedGrants, "warning", map[string]any{
		"member": "dana@x.test",
		"groups": []string{"binmgr", "configmgr"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := NewQueue(db, user.ID).Drain(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d notices, want 1", len(got))
	}
	if !strings.Contains(got[0].Title, "dana@x.test") {
		t.Errorf("title should name the member; got %q", got[0].Title)
	}
	if !strings.Contains(got[0].Body, "binmgr, configmgr") {
		t.Errorf("body should list the retained groups; got %q", got[0].Body)
	}
}

// TestNoticeQueue_EmptyUserNoOp: signaling on a userID="" queue is a
// silent no-op (system invocation path).
func TestNoticeQueue_EmptyUserNoOp(t *testing.T) {
	db := testStore(t)

	q := NewQueue(db, "")
	q.Maybe(context.Background(), "tmp_48h_expiry")
	if got := len(q.Drain(context.Background())); got != 0 {
		t.Errorf("empty userID: got %d queued, want 0", got)
	}
}

// TestNoticeQueue_UnknownKeyNoOp: signaling a key that has no
// registry entry is a silent skip — registry/code drift shouldn't
// crash a request.
func TestNoticeQueue_UnknownKeyNoOp(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-nq4@example.test")

	q := NewQueue(db, user.ID)
	q.Maybe(context.Background(), "not_a_real_key")
	if got := len(q.Drain(context.Background())); got != 0 {
		t.Errorf("unknown key: got %d queued, want 0", got)
	}
}

// TestNoticeQueue_UnknownKeyStaysPending: a row whose key the running
// binary doesn't know stays queued rather than being burned. A deploy
// that adds the registry entry then delivers it.
func TestNoticeQueue_UnknownKeyStaysPending(t *testing.T) {
	db := testStore(t)
	user, _ := db.CreateUser("alice-drift@example.test")

	if err := db.EmitNotice(user.ID, "from_a_newer_build", "warning", nil); err != nil {
		t.Fatal(err)
	}
	if got := len(NewQueue(db, user.ID).Drain(context.Background())); got != 0 {
		t.Fatalf("unrenderable notice should not be delivered; got %d", got)
	}
	pending, err := db.PendingNotices(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("unrenderable notice should stay pending; got %d", len(pending))
	}
}

// TestNoticeQueue_PerUser: draining alice's queue leaves bob's alone.
func TestNoticeQueue_PerUser(t *testing.T) {
	db := testStore(t)
	alice, _ := db.CreateUser("alice-pu2@example.test")
	bob, _ := db.CreateUser("bob-pu2@example.test")

	qA := NewQueue(db, alice.ID)
	qA.Maybe(context.Background(), "tmp_48h_expiry")
	qA.Drain(context.Background())

	qB := NewQueue(db, bob.ID)
	qB.Maybe(context.Background(), "tmp_48h_expiry")
	if got := len(qB.Drain(context.Background())); got != 1 {
		t.Errorf("bob's signal: got %d queued, want 1 (alice's drain should not affect bob)", got)
	}
}

// TestRender_UnknownKey: rendering is the drift boundary — an
// unknown key reports false rather than producing a blank card.
func TestRender_UnknownKey(t *testing.T) {
	if _, ok := Render(store.PendingNotice{Key: "nope"}); ok {
		t.Error("Render(unknown key) reported ok")
	}
}
