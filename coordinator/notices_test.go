package coordinator

import (
	"testing"
)

// TestNoticeQueue_FirstSignalQueues: a fresh queue with an unseen
// notice should queue and flush exactly one entry.
func TestNoticeQueue_FirstSignalQueues(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	user, _ := db.CreateUser("alice-nq1@example.test")

	q := NewNoticeQueue(db, user.ID)
	q.Maybe("tmp_48h_expiry")

	notices := q.Flush()
	if len(notices) != 1 {
		t.Fatalf("first flush: got %d notices, want 1", len(notices))
	}
	if notices[0].Key != "tmp_48h_expiry" {
		t.Errorf("notice key: got %q, want tmp_48h_expiry", notices[0].Key)
	}
	if notices[0].Kind != NoticeKindWarning {
		t.Errorf("notice kind: got %q, want warning", notices[0].Kind)
	}
}

// TestNoticeQueue_MaybeIsIdempotent: signaling the same key
// repeatedly within a session queues at most once.
func TestNoticeQueue_MaybeIsIdempotent(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	user, _ := db.CreateUser("alice-nq2@example.test")

	q := NewNoticeQueue(db, user.ID)
	q.Maybe("tmp_48h_expiry")
	q.Maybe("tmp_48h_expiry")
	q.Maybe("tmp_48h_expiry")

	if got := len(q.Flush()); got != 1 {
		t.Errorf("multiple Maybe → got %d queued, want 1", got)
	}
}

// TestNoticeQueue_FlushMarksSeen: after Flush, the same key in a new
// session does NOT queue (it was marked seen).
func TestNoticeQueue_FlushMarksSeen(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	user, _ := db.CreateUser("alice-nq3@example.test")

	q1 := NewNoticeQueue(db, user.ID)
	q1.Maybe("tmp_48h_expiry")
	q1.Flush()

	// Fresh queue, same user — should silent-skip.
	q2 := NewNoticeQueue(db, user.ID)
	q2.Maybe("tmp_48h_expiry")
	if got := len(q2.Flush()); got != 0 {
		t.Errorf("second-session signal: got %d queued, want 0 (already seen)", got)
	}
}

// TestNoticeQueue_EmptyUserNoOp: signaling on a userID="" queue is a
// silent no-op (system invocation path).
func TestNoticeQueue_EmptyUserNoOp(t *testing.T) {
	db, _ := testStoreAndCustomer(t)

	q := NewNoticeQueue(db, "")
	q.Maybe("tmp_48h_expiry")
	if got := len(q.Flush()); got != 0 {
		t.Errorf("empty userID: got %d queued, want 0", got)
	}
}

// TestNoticeQueue_UnknownKeyNoOp: signaling a key that has no
// registry entry is a silent skip — registry/code drift shouldn't
// crash a request.
func TestNoticeQueue_UnknownKeyNoOp(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	user, _ := db.CreateUser("alice-nq4@example.test")

	q := NewNoticeQueue(db, user.ID)
	q.Maybe("not_a_real_key")
	if got := len(q.Flush()); got != 0 {
		t.Errorf("unknown key: got %d queued, want 0", got)
	}
}

// TestNoticeQueue_PerUser: alice's seen state doesn't burn bob's.
func TestNoticeQueue_PerUser(t *testing.T) {
	db, _ := testStoreAndCustomer(t)
	alice, _ := db.CreateUser("alice-pu2@example.test")
	bob, _ := db.CreateUser("bob-pu2@example.test")

	// Alice sees + dismisses.
	qA := NewNoticeQueue(db, alice.ID)
	qA.Maybe("tmp_48h_expiry")
	qA.Flush()

	// Bob signals → still gets a notice.
	qB := NewNoticeQueue(db, bob.ID)
	qB.Maybe("tmp_48h_expiry")
	if got := len(qB.Flush()); got != 1 {
		t.Errorf("bob's first signal: got %d queued, want 1 (alice's flush should not affect bob)", got)
	}
}
