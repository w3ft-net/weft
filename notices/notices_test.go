package notices

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/sqlitedb"
)

func testStore(t *testing.T) notice.Store {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := defaults.NewNoticeBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var ctx = context.Background()

// A signaled notice drains once.
func TestNoticeQueue_FirstSignalQueues(t *testing.T) {
	q := NewQueue(testStore(t), "u1")
	q.Maybe(ctx, "tmp_48h_expiry")

	got := q.Drain(ctx)
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

// Signaling the same key repeatedly within one request queues at most once. A
// pipeline forks a goroutine per stage, so without this a single command could
// write hundreds of identical rows.
func TestNoticeQueue_MaybeDedupsWithinRequest(t *testing.T) {
	q := NewQueue(testStore(t), "u1")
	for i := 0; i < 3; i++ {
		q.Maybe(ctx, "tmp_48h_expiry")
	}
	if got := len(q.Drain(ctx)); got != 1 {
		t.Errorf("multiple Maybe → got %d queued, want 1", got)
	}
}

// A drained notice does not come back on the next request, but a fresh signal
// does.
func TestNoticeQueue_DrainDeliversOnce(t *testing.T) {
	st := testStore(t)
	q1 := NewQueue(st, "u1")
	q1.Maybe(ctx, "tmp_48h_expiry")
	if got := len(q1.Drain(ctx)); got != 1 {
		t.Fatalf("first drain: got %d, want 1", got)
	}
	if got := len(NewQueue(st, "u1").Drain(ctx)); got != 0 {
		t.Errorf("drain with nothing signaled: got %d, want 0", got)
	}
	q2 := NewQueue(st, "u1")
	q2.Maybe(ctx, "tmp_48h_expiry")
	if got := len(q2.Drain(ctx)); got != 1 {
		t.Errorf("re-signal in a new request: got %d, want 1", got)
	}
}

// The whole point of the queue: a notice emitted with no request in flight, as a
// database trigger does, reaches the user on their next command, and the
// emitter knows nothing about delivery. The notice here is one a deployment
// registers itself.
func TestNoticeQueue_DrainsOutOfBandNotices(t *testing.T) {
	Register("test_oob_notice", KindWarning, func(p map[string]any) (string, string) {
		who, _ := p["member"].(string)
		return "Heads up about " + who, "Out of band."
	})
	st := testStore(t)
	if err := st.Emit("u1", "test_oob_notice", notice.KindWarning, map[string]any{"member": "dana@x.test"}); err != nil {
		t.Fatal(err)
	}
	got := NewQueue(st, "u1").Drain(ctx)
	if len(got) != 1 || got[0].Title != "Heads up about dana@x.test" || got[0].Body != "Out of band." {
		t.Fatalf("drained %+v", got)
	}
}

func TestRegister_TwiceIsAWiringMistake(t *testing.T) {
	Register("test_dup_notice", KindSuccess, func(map[string]any) (string, string) { return "", "" })
	defer func() {
		if recover() == nil {
			t.Error("registering a key twice did not panic")
		}
	}()
	Register("test_dup_notice", KindSuccess, func(map[string]any) (string, string) { return "", "" })
}

// Signaling on a userID="" queue is a silent no-op (the system invocation path).
func TestNoticeQueue_EmptyUserNoOp(t *testing.T) {
	q := NewQueue(testStore(t), "")
	q.Maybe(ctx, "tmp_48h_expiry")
	if got := len(q.Drain(ctx)); got != 0 {
		t.Errorf("empty userID: got %d queued, want 0", got)
	}
}

// Signaling a key that has no registry entry is a silent skip: registry/code
// drift shouldn't crash a request.
func TestNoticeQueue_UnknownKeyNoOp(t *testing.T) {
	q := NewQueue(testStore(t), "u1")
	q.Maybe(ctx, "not_a_real_key")
	if got := len(q.Drain(ctx)); got != 0 {
		t.Errorf("unknown key: got %d queued, want 0", got)
	}
}

// A row whose key the running binary doesn't know stays queued rather than being
// burned. A deploy that adds the registry entry then delivers it.
func TestNoticeQueue_UnknownKeyStaysPending(t *testing.T) {
	st := testStore(t)
	if err := st.Emit("u1", "from_a_newer_build", notice.KindWarning, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(NewQueue(st, "u1").Drain(ctx)); got != 0 {
		t.Fatalf("unrenderable notice should not be delivered; got %d", got)
	}
	if pending, err := st.Pending("u1"); err != nil || len(pending) != 1 {
		t.Errorf("unrenderable notice should stay pending; got %d, %v", len(pending), err)
	}
}

// Draining one user's queue leaves another's alone.
func TestNoticeQueue_PerUser(t *testing.T) {
	st := testStore(t)
	qA := NewQueue(st, "alice")
	qA.Maybe(ctx, "tmp_48h_expiry")
	qA.Drain(ctx)

	qB := NewQueue(st, "bob")
	qB.Maybe(ctx, "tmp_48h_expiry")
	if got := len(qB.Drain(ctx)); got != 1 {
		t.Errorf("bob's signal: got %d queued, want 1 (alice's drain should not affect bob)", got)
	}
}

// Rendering is the drift boundary: an unknown key reports false rather than
// producing a blank card.
func TestRender_UnknownKey(t *testing.T) {
	if _, ok := Render(notice.Pending{Key: "nope"}); ok {
		t.Error("Render(unknown key) reported ok")
	}
}
