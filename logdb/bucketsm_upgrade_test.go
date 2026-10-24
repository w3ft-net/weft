//go:build test

package logdb

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// seedUpgradingRow seeds an UPGRADING shard mid-move whose upgrade returns
// it to origin.
func seedUpgradingRow(t *testing.T, db *store.Store, bucketID int64, id string, origin ShardState, windowStart time.Time, stripe *int) {
	t.Helper()
	seedShardRow(t, db, bucketID, id, StateUpgrading, windowStart, stripe)
	at := windowStart
	if err := (ShardStore{db}).SetShardUpgrade(id, UpgradeFacts{Phase: PhaseSwapping, Origin: origin, PhaseAt: &at}); err != nil {
		t.Fatal(err)
	}
}

// An outgoing shard caught mid-upgrade is still part of the write cohort
// (its writes are held, not rerouted), so the rotation must rotate it too
// — otherwise it returns to INGESTING in a window the bucket has left.
func TestBucketSM_RotationRotatesAShardMidUpgrade(t *testing.T) {
	sm, db, ab, ctrl := makeFixtureBucketSM(t, BucketWarming)
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedShardRow(t, db, ab.ID, "s-next-1", StateIngesting, nextWin, ip(1))
	seedShardRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))
	seedUpgradingRow(t, db, ab.ID, "s-cur-1", StateIngesting, curWin, ip(1))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	rotated := ctrl.firedFor(TriggerRotateRequested)
	sort.Strings(rotated)
	if len(rotated) != 2 || rotated[1] != "s-cur-1" {
		t.Fatalf("rotated: %v, want both outgoing shards including the upgrading s-cur-1", rotated)
	}
}

// The rotation isn't done while a mid-upgrade outgoing shard would still
// return to INGESTING; once its origin is READ_ONLY it is.
func TestBucketSM_RotatingWaitsForAShardMidUpgrade(t *testing.T) {
	sm, db, ab, _ := makeFixtureBucketSM(t, BucketRotating)
	seedShardRow(t, db, ab.ID, "s-next-0", StateIngesting, nextWin, ip(0))
	seedUpgradingRow(t, db, ab.ID, "s-cur-0", StateIngesting, curWin, ip(0))

	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := sm.State(context.Background()); got != BucketRotating {
		t.Fatalf("state: %q, want ROTATING (the upgrading shard would return to INGESTING)", got)
	}
	at := curWin
	if err := (ShardStore{db}).SetShardUpgrade("s-cur-0", UpgradeFacts{Phase: PhaseSwapping, Origin: StateReadOnly, PhaseAt: &at}); err != nil {
		t.Fatal(err)
	}
	if err := sm.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := sm.State(context.Background()); got != BucketStasis {
		t.Fatalf("state: %q, want STASIS once the upgrading shard will land READ_ONLY", got)
	}
}
