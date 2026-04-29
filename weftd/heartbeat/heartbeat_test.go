package heartbeat

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHit_StoresLastSeen(t *testing.T) {
	h := New(10, time.Hour)
	at := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return at }

	if !h.Hit("svc-a") {
		t.Fatal("Hit returned false for valid name")
	}

	snaps := h.Snapshot()
	if len(snaps) != 1 || snaps[0].Service != "svc-a" || !snaps[0].LastSeen.Equal(at) {
		t.Errorf("snapshot = %+v, want one entry for svc-a at %v", snaps, at)
	}
}

func TestHit_LastWriteWins(t *testing.T) {
	h := New(10, time.Hour)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return t0 }
	h.Hit("svc")
	t1 := t0.Add(5 * time.Second)
	h.now = func() time.Time { return t1 }
	h.Hit("svc")

	snaps := h.Snapshot()
	if len(snaps) != 1 || !snaps[0].LastSeen.Equal(t1) {
		t.Errorf("snapshot = %+v, want LastSeen=%v", snaps, t1)
	}
}

func TestHit_RejectsInvalidNames(t *testing.T) {
	h := New(10, time.Hour)
	for _, bad := range []string{
		"",
		"with space",
		"with/slash",
		"semi;colon",
		strings.Repeat("a", 129),
	} {
		if h.Hit(bad) {
			t.Errorf("Hit(%q) returned true; want false", bad)
		}
	}
	for _, good := range []string{
		"svc-a",
		"my.service",
		"under_score",
		"123",
		"a",
		strings.Repeat("a", 128),
	} {
		if !h.Hit(good) {
			t.Errorf("Hit(%q) returned false; want true", good)
		}
	}
}

func TestSnapshot_DropsStaleEntries(t *testing.T) {
	h := New(10, 30*time.Second)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	h.now = func() time.Time { return t0 }
	h.Hit("fresh")
	h.now = func() time.Time { return t0.Add(-2 * time.Minute) }
	h.Hit("stale")

	// Snapshot at t0 — "stale" is 2min old, > 30s staleAfter.
	h.now = func() time.Time { return t0 }
	snaps := h.Snapshot()
	if len(snaps) != 1 || snaps[0].Service != "fresh" {
		t.Errorf("snapshot = %+v, want only 'fresh'", snaps)
	}
	if h.Len() != 1 {
		t.Errorf("Len = %d, want 1 (stale should be evicted in-place)", h.Len())
	}
}

func TestHit_LRUEvictionAtCap(t *testing.T) {
	h := New(3, time.Hour)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	for i, name := range []string{"a", "b", "c"} {
		h.now = func() time.Time { return t0.Add(time.Duration(i) * time.Second) }
		h.Hit(name)
	}
	if h.Len() != 3 {
		t.Fatalf("Len after 3 hits at cap=3 = %d, want 3", h.Len())
	}

	// Adding "d" should evict "a" (oldest).
	h.now = func() time.Time { return t0.Add(10 * time.Second) }
	h.Hit("d")

	got := map[string]bool{}
	for _, s := range h.Snapshot() {
		got[s.Service] = true
	}
	if got["a"] {
		t.Error("expected 'a' evicted, but it's still present")
	}
	for _, want := range []string{"b", "c", "d"} {
		if !got[want] {
			t.Errorf("expected %q present, missing", want)
		}
	}
}

func TestHit_RehitDoesNotEvict(t *testing.T) {
	// Re-hitting an existing service at cap shouldn't trigger eviction.
	h := New(2, time.Hour)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	h.now = func() time.Time { return t0 }
	h.Hit("a")
	h.now = func() time.Time { return t0.Add(1 * time.Second) }
	h.Hit("b")
	h.now = func() time.Time { return t0.Add(2 * time.Second) }
	h.Hit("a") // bump a — would evict if treated as new
	if h.Len() != 2 {
		t.Errorf("Len = %d, want 2 (a was rehit, not added)", h.Len())
	}
}

func TestHandler_AcceptsValidPost(t *testing.T) {
	h := New(10, time.Hour)
	h.now = func() time.Time { return time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC) }

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/h/my-svc", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if h.Len() != 1 {
		t.Errorf("Len = %d, want 1", h.Len())
	}
}

func TestHandler_RejectsBadName(t *testing.T) {
	h := New(10, time.Hour)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/h/with%20space", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid name", resp.StatusCode)
	}
}

func TestHandler_RejectsWrongMethod(t *testing.T) {
	h := New(10, time.Hour)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/h/foo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestHandler_RejectsWrongPath(t *testing.T) {
	h := New(10, time.Hour)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/notheartbeat/foo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHandler_BodyIgnored(t *testing.T) {
	// Spec says body is ignored; arbitrary content shouldn't change anything.
	h := New(10, time.Hour)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/h/svc", "application/json", strings.NewReader(`{"random":"junk"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (body should be ignored)", resp.StatusCode)
	}
}
