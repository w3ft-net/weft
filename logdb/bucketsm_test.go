package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// fakeBucketSMStore is the in-memory BucketSMStore used by bucket SM
// tests. Holds one Bucket row + a slice of ShardRecords for the
// per-window-bound lookups Evaluate uses.
type fakeBucketSMStore struct {
	mu      sync.Mutex
	buckets map[int64]*store.Bucket
	shards  []store.ShardRecord
}

func newFakeBucketSMStore() *fakeBucketSMStore {
	return &fakeBucketSMStore{buckets: map[int64]*store.Bucket{}}
}

func (f *fakeBucketSMStore) putBucket(b store.Bucket) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := b
	f.buckets[b.ID] = &cp
}

func (f *fakeBucketSMStore) GetBucket(id int64) (store.Bucket, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[id]
	if !ok {
		return store.Bucket{}, false, nil
	}
	return *b, true, nil
}

func (f *fakeBucketSMStore) UpdateBucketState(id int64, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.buckets[id]; ok {
		b.State = state
	}
	return nil
}

func (f *fakeBucketSMStore) UpdateCascadeStartedAt(id int64, t *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.buckets[id]; ok {
		b.CascadeStartedAt = t
	}
	return nil
}

func (f *fakeBucketSMStore) ShardsInBucketWithWindow(bucketID int64, windowStart time.Time, states []string) ([]store.ShardRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ShardRecord
	ts := windowStart.UTC().Format(time.RFC3339)
	stateSet := map[string]struct{}{}
	for _, s := range states {
		stateSet[s] = struct{}{}
	}
	for _, r := range f.shards {
		if r.BucketID != bucketID {
			continue
		}
		if r.WindowStart != ts {
			continue
		}
		if _, ok := stateSet[r.State]; !ok {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeBucketSMStore) ShardsInBucketBeforeWindow(bucketID int64, windowStart time.Time, states []string) ([]store.ShardRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ShardRecord
	stateSet := map[string]struct{}{}
	for _, s := range states {
		stateSet[s] = struct{}{}
	}
	for _, r := range f.shards {
		if r.BucketID != bucketID {
			continue
		}
		rts, err := time.Parse(time.RFC3339, r.WindowStart)
		if err != nil || !rts.Before(windowStart.UTC()) {
			continue
		}
		if _, ok := stateSet[r.State]; !ok {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// makeFixtureBucketSM builds a BucketSM in initialState. Returns the
// SM, the fake store, and the AppBucket the SM holds.
func makeFixtureBucketSM(t *testing.T, initialState BucketState) (*BucketSM, *fakeBucketSMStore, *AppBucket) {
	t.Helper()
	fstore := newFakeBucketSMStore()
	fstore.putBucket(store.Bucket{
		ID:                   1,
		Label:                "default",
		Tier:                 "small",
		TargetIngestingCount: 2,
		State:                string(initialState),
	})
	ab := &AppBucket{ID: 1, Label: "default", Tier: "small", Target: 2, WindowDuration: time.Hour}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	sm := NewBucketSM(ab, BucketSMDeps{
		Store: fstore,
		Clock: clock,
	})
	return sm, fstore, ab
}

func TestBucketSM_StasisToWarmingOnRotationNeeded(t *testing.T) {
	sm, fstore, _ := makeFixtureBucketSM(t, BucketStasis)

	if err := sm.Fire(context.Background(), TriggerRotationNeeded); err != nil {
		t.Fatalf("Fire RotationNeeded: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketWarming {
		t.Fatalf("state: got %q want WARMING", got)
	}
	// cascade_started_at must be set on WARMING entry
	b, _, _ := fstore.GetBucket(1)
	if b.CascadeStartedAt == nil {
		t.Fatalf("cascade_started_at: nil; expected set on WARMING")
	}
}

func TestBucketSM_FullCascade(t *testing.T) {
	sm, fstore, _ := makeFixtureBucketSM(t, BucketStasis)
	ctx := context.Background()

	if err := sm.Fire(ctx, TriggerRotationNeeded); err != nil {
		t.Fatalf("RotationNeeded: %v", err)
	}
	if err := sm.Fire(ctx, TriggerAllReplacementsReady); err != nil {
		t.Fatalf("AllReplacementsReady: %v", err)
	}
	if err := sm.Fire(ctx, TriggerOldShardsRotated); err != nil {
		t.Fatalf("OldShardsRotated: %v", err)
	}
	if err := sm.Fire(ctx, TriggerArchivalsComplete); err != nil {
		t.Fatalf("ArchivalsComplete: %v", err)
	}

	got, err := sm.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketStasis {
		t.Fatalf("state: got %q want STASIS", got)
	}

	// cascade_started_at must be cleared on STASIS re-entry
	b, _, _ := fstore.GetBucket(1)
	if b.CascadeStartedAt != nil {
		t.Fatalf("cascade_started_at: %v; expected nil after cascade", b.CascadeStartedAt)
	}
}

func TestBucketSM_ScaleDownToStasis(t *testing.T) {
	sm, _, _ := makeFixtureBucketSM(t, BucketStasis)
	ctx := context.Background()

	if err := sm.Fire(ctx, TriggerScaleDownRequested); err != nil {
		t.Fatalf("ScaleDownRequested: %v", err)
	}
	if err := sm.Fire(ctx, TriggerSettled); err != nil {
		t.Fatalf("Settled: %v", err)
	}
	got, err := sm.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketStasis {
		t.Fatalf("state: got %q want STASIS", got)
	}
}

func TestBucketSM_StasisRejectsCascadeAdvanceTriggers(t *testing.T) {
	// STASIS only accepts the cascade-start triggers; firing an
	// advance trigger from STASIS is a logic error and must error.
	for _, trigger := range []Trigger{
		TriggerAllReplacementsReady,
		TriggerOldShardsRotated,
		TriggerArchivalsComplete,
		TriggerSettled,
	} {
		t.Run(string(trigger), func(t *testing.T) {
			sm, _, _ := makeFixtureBucketSM(t, BucketStasis)
			err := sm.Fire(context.Background(), trigger)
			if err == nil {
				t.Fatalf("expected %q to be rejected from STASIS", trigger)
			}
		})
	}
}

func TestBucketSM_HydratesStateFromStore(t *testing.T) {
	// Construct SM with a bucket row already in ROTATING.
	sm, _, _ := makeFixtureBucketSM(t, BucketRotating)
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != BucketRotating {
		t.Fatalf("state: got %q want ROTATING (hydrated from store)", got)
	}
	// And the appropriate next-step trigger fires.
	if err := sm.Fire(context.Background(), TriggerOldShardsRotated); err != nil {
		t.Fatalf("Fire OldShardsRotated after hydrate: %v", err)
	}
}
