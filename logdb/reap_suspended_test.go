package logdb

import (
	"errors"
	"testing"
	"time"
)

// TestReapStrandedSuspended pins the stranded-SUSPENDED reaper.
//
// SUSPENDED shards whose rebind never completed accumulate forever
// (prod showed several days-old, holding data, plus 0-row ghosts). We
// may safely delete the row ONLY when deleting it can't lose data that
// isn't already lost or preserved elsewhere:
//
//   - volume gone / re-homed under another shard (FindByShard by this
//     shard's id returns nil) → safe to delete.
//   - volume still exists and tagged to this shard (FindByShard returns
//     it) → this row is the sole pointer to live data → MUST keep
//     (left for the rebind path).
//   - volume lookup errors → unknown → MUST keep (fail safe).
//   - non-SUSPENDED states are out of scope entirely.
func TestReapStrandedSuspended(t *testing.T) {
	reg := NewShardRegistry()
	fakeStore := newFakeRegistryStore()
	reg.SetStore(fakeStore)

	old := time.Now().Add(-time.Hour)
	reg.Add(&ShardEntry{ID: "susp-gone", State: StateSuspended, EBSVolumeID: "vol-gone", CreatedAt: old})
	reg.Add(&ShardEntry{ID: "susp-live", State: StateSuspended, EBSVolumeID: "vol-live", CreatedAt: old})
	reg.Add(&ShardEntry{ID: "susp-err", State: StateSuspended, EBSVolumeID: "vol-err", CreatedAt: old})
	reg.Add(&ShardEntry{ID: "ingest-ok", State: StateIngesting, Addr: "10.0.0.1:9000", CreatedAt: old})

	vm := &fakeVolumeManager{
		findResult: map[string]*VolumeInfo{
			"susp-live": {VolumeID: "vol-live", State: "available"},
		},
		findErr: map[string]error{
			"susp-err": errors.New("ec2 throttled"),
		},
	}
	prov := &Provisioner{
		registry: reg,
		volumes:  vm,
		config:   ProvisionerConfig{ShardOps: fakeStore},
	}

	prov.reapStrandedSuspended()

	// susp-gone: volume absent → deleted from registry AND store.
	if reg.Get("susp-gone") != nil {
		t.Error("susp-gone (volume gone) should have been removed from the registry")
	}
	if _, ok, _ := fakeStore.GetShard("susp-gone"); ok {
		t.Error("susp-gone should have been deleted from shard_records")
	}

	// susp-live: volume still tagged to it → kept (sole pointer to data).
	if reg.Get("susp-live") == nil {
		t.Error("susp-live (volume still present) must NOT be reaped — it is the only pointer to its data")
	}

	// susp-err: lookup failed → fail safe, kept.
	if reg.Get("susp-err") == nil {
		t.Error("susp-err (volume lookup errored) must NOT be reaped — fail safe")
	}

	// ingest-ok: not SUSPENDED → untouched.
	if reg.Get("ingest-ok") == nil {
		t.Error("non-SUSPENDED shard must never be touched by the SUSPENDED reaper")
	}
}
