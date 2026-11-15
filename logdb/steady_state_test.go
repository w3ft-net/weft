package logdb

import (
	"testing"
	"time"
)

// TestRefreshFromStore_PicksUpExternalDBChanges verifies that a
// manual DB edit (e.g. updating window_end to force a rotation) is
// visible to the registry after RefreshFromStore, and that the next
// reconciler tick acts on the updated value.
func TestRefreshFromStore_PicksUpExternalDBChanges(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	now := time.Now().UTC()
	futureEnd := now.Add(7 * 24 * time.Hour) // 7-day window

	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:          "shard-legacy",
			BucketID:    1,
			State:       StateIngesting,
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   futureEnd,
		},
		Live: &failingShard{id: "shard-legacy"},
	})

	// Verify the in-memory entry has the 7-day window.
	if got := reg.Get("shard-legacy").WindowEnd; !got.Equal(futureEnd) {
		t.Fatalf("pre-edit WindowEnd: got %v want %v", got, futureEnd)
	}

	// Simulate an operator editing the DB directly.
	// Truncate to seconds: RFC3339 round-trips lose sub-second precision.
	closedEnd := now.Add(-time.Minute).Truncate(time.Second)
	rec, ok := fake.get("shard-legacy")
	if !ok {
		t.Fatal("shard-legacy not in store")
	}
	rec.WindowEnd = closedEnd
	fake.mu.Lock()
	fake.records["shard-legacy"] = rec
	fake.mu.Unlock()

	// Before refresh: in-memory still has old window.
	if got := reg.Get("shard-legacy").WindowEnd; got.Equal(closedEnd) {
		t.Fatal("in-memory entry should NOT reflect DB edit before refresh")
	}

	reg.RefreshFromStore(false)

	// After refresh: in-memory matches the DB edit, so the next FSM tick
	// sees the closed window and rotates the shard. (The rotation itself
	// is covered by bucketsm_test; here we only assert the refresh lands.)
	if got := reg.Get("shard-legacy").WindowEnd; !got.Equal(closedEnd) {
		t.Fatalf("post-refresh WindowEnd: got %v want %v", got, closedEnd)
	}
}

// TestRefreshFromStore_DropsDeletedRowsOnlyWhenAsked pins the follower half
// of shard removal: the leader deletes a GONE shard's row and its own entry
// (onEnterGone), but a follower only learns of it from the store. With
// dropDeleted it lets the entry go and closes its handle; without it (the
// leader's refresh) the entry stays, since a just-minted shard is in the
// leader's map before its row is written.
func TestRefreshFromStore_DropsDeletedRowsOnlyWhenAsked(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	live := &fakeArchivableShard{id: "shard-gone"}
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:       "shard-gone",
			BucketID: 1,
			State:    StateArchived,
		},
		Live: live,
	})
	fake.mu.Lock()
	delete(fake.records, "shard-gone")
	fake.mu.Unlock()

	reg.RefreshFromStore(false)
	if reg.Get("shard-gone") == nil {
		t.Fatal("leader refresh must not drop an entry whose row is missing")
	}

	reg.RefreshFromStore(true)
	if reg.Get("shard-gone") != nil {
		t.Fatal("follower refresh should drop an entry whose row was deleted")
	}
	if !live.closed {
		t.Error("dropped entry's live handle was not closed")
	}
}

// TestRefreshFromStore_PreservesShardPointer ensures that
// RefreshFromStore updates metadata but does not clobber the live
// Shard (gRPC connection) pointer.
func TestRefreshFromStore_PreservesShardPointer(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	sh := &failingShard{id: "shard-x"}
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:       "shard-x",
			BucketID: 1,
			State:    StateIngesting,
		},
		Live: sh,
	})

	// Mutate a metadata field in the DB.
	rec, _, _ := fake.GetShard("shard-x")
	rec.Tier = "large"
	fake.mu.Lock()
	fake.records["shard-x"] = rec
	fake.mu.Unlock()

	reg.RefreshFromStore(false)

	entry := reg.Get("shard-x")
	if entry.Tier != "large" {
		t.Errorf("Tier not refreshed: got %q want large", entry.Tier)
	}
	if entry.Live != sh {
		t.Error("Shard pointer was clobbered by RefreshFromStore")
	}
}

// assertNoWindowOverlap checks that no INGESTING shard's window
// overlaps with any READ_ONLY shard's window. Shards with the same
// window and same state are expected (target=2 → 2 shards per window).
func assertNoWindowOverlap(t *testing.T, reg *ShardRegistry, label string) {
	t.Helper()
	type window struct {
		id    string
		state ShardState
		start time.Time
		end   time.Time
	}
	var windows []window
	for _, e := range reg.Entries() {
		if e.State == StateIngesting || e.State == StateReadOnly {
			windows = append(windows, window{e.ID, e.State, e.WindowStart, e.WindowEnd})
		}
	}
	for i, a := range windows {
		for j, b := range windows {
			if i >= j {
				continue
			}
			if a.start.Before(b.end) && b.start.Before(a.end) {
				if a.start.Equal(b.start) && a.end.Equal(b.end) && a.state == b.state {
					continue
				}
				t.Errorf("%s: overlapping windows: %s(%s) [%s,%s) and %s(%s) [%s,%s)",
					label,
					a.id, a.state, a.start.Format(time.RFC3339), a.end.Format(time.RFC3339),
					b.id, b.state, b.start.Format(time.RFC3339), b.end.Format(time.RFC3339))
			}
		}
	}
}
