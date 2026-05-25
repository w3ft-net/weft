package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// makeFixtureBucketSM builds a BucketSM in initialState backed by a
// real (transaction-scoped) store. Seeds the buckets row, then returns
// the SM, the store, and the Bucket the SM holds. openSMTestStore +
// seedBucket live in shardsm_test.go.
func makeFixtureBucketSM(t *testing.T, initialState BucketState) (*BucketSM, *store.Store, *Bucket) {
	t.Helper()
	db := openSMTestStore(t)
	bucket := seedBucket(t, db, initialState)
	ab := &Bucket{ID: bucket.ID, Label: bucket.Label, Tier: "small", Target: 2, WindowDuration: time.Hour}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	sm := NewBucketSM(ab, db.DB(), clock)
	return sm, db, ab
}

func TestBucketSM_StasisToWarmingOnRotationNeeded(t *testing.T) {
	sm, db, ab := makeFixtureBucketSM(t, BucketStasis)

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
	b, _, _ := db.GetBucket(ab.ID)
	if b.CascadeStartedAt == nil {
		t.Fatalf("cascade_started_at: nil; expected set on WARMING")
	}
}

func TestBucketSM_FullCascade(t *testing.T) {
	sm, db, ab := makeFixtureBucketSM(t, BucketStasis)
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
	b, _, _ := db.GetBucket(ab.ID)
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
