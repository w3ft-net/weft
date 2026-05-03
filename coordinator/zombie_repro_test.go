package coordinator

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// These three tests pin the bug-trio behind the prod orphan-christen
// loop seen on 2026-04-29:
//
//   1) reapDeadShards wipes a dead shard's instance_id and addr in the
//      DB, expecting a rebind that may never come. There is no
//      timeout, so a failed rebind leaves a row in state=INGESTING
//      with no instance forever — a zombie.
//   2) The reconciler reads IngestingCount from the DB (counts the
//      zombie) but walks Entries from the registry (doesn't see the
//      zombie). The asymmetry means Op 2 (under-capacity) thinks the
//      target is met while Op 3 (rotate-at-windowEnd) never fires for
//      the missing entry.
//   3) cmd/coordinator/main.go's LoadFromStore reconstruct func only
//      restores ARCHIVED shards. After any coordinator restart, every
//      live shard (INGESTING / READ_ONLY / LAUNCHING) vanishes from
//      the in-memory registry — converting any transient (1)-shaped
//      condition into a permanent zombie.
//
// All three tests are expected to fail against the current code.
// They are reproducers, not regressions; the fix lands once the
// registry becomes a thin DB-cache and the LoadFromStore filter is
// removed, after which all three pass without modification.

// (1) reapDeadShards wipes instance_id and addr in the DB so a future
// rebind can re-stamp them. No timeout exists. If the rebind never
// happens (volume gone, lifecycle hook claims a different LAUNCHING
// row, etc.), the row sits in INGESTING-with-no-instance until
// someone opens psql.
//
// What we want: after some "reasonable" time without a rebind, the
// reaper (or some other path) should give up and transition the row
// out of INGESTING — either to ARCHIVED (if the volume is gone) or
// back to LAUNCHING (so the reconciler/lifecycle hook can re-claim
// it). What happens today: nothing.
func TestZombie_WipeAndAwaitRebind_PersistsForever(t *testing.T) {
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	const id = "shard-20260420T0000-zombie"
	reg.Add(&ShardEntry{
		ID:          id,
		State:       StateIngesting,
		Addr:        "10.0.0.10:9000",
		InstanceID:  "i-original",
		EBSVolumeID: "vol-still-around",
		WindowStart: time.Now().Add(-2 * 24 * time.Hour),
		WindowEnd:   time.Now().Add(5 * 24 * time.Hour),
		CreatedAt:   time.Now().Add(-2 * 24 * time.Hour),
	})

	// First reaper pass — the heartbeat is stale and there's a bound
	// volume, so it goes down the wipe-and-await-rebind branch. The
	// dead instance is asked-for-replacement; the registry entry is
	// preserved with empty instance_id/addr; the DB row is updated
	// to match.
	mockStore := &mockHeartbeatStore{
		heartbeats: []store.ShardHeartbeat{
			{ShardID: "other-live", Addr: "10.0.0.1:9000", State: "INGESTING"},
		},
	}
	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config:   ProvisionerConfig{Store: mockStore, PollInterval: time.Second},
	}
	prov.reapDeadShards()

	// Confirm: the wipe landed. State should now be SUSPENDED (not
	// INGESTING) so the reconciler doesn't count it toward target.
	rec, ok := fake.get(id)
	if !ok {
		t.Fatal("zombie row missing after wipe; expected the reaper to preserve it")
	}
	if rec.State != string(StateSuspended) {
		t.Fatalf("state after wipe = %q, expected SUSPENDED", rec.State)
	}
	if rec.InstanceID != "" || rec.Addr != "" {
		t.Fatalf("instance_id/addr should be wiped (await rebind); got %q / %q",
			rec.InstanceID, rec.Addr)
	}

	// Simulate "rebind never happens": time passes, no replacement
	// instance heartbeats with this shard ID. The reaper runs many
	// more times — in prod this is ~30s × N hours.
	for i := 0; i < 50; i++ {
		prov.reapDeadShards()
	}

	// SUSPENDED entries persist until a RECOVER rebinds them. No
	// timeout-based archival — the volume stays recoverable. The key
	// property: SUSPENDED does NOT count toward IngestingCount, so the
	// reconciler isn't blocked.
	rec, _ = fake.get(id)
	if rec.State != string(StateSuspended) {
		t.Errorf("after 50 reaper ticks, shard %s state=%s — expected SUSPENDED (no timeout archival)",
			id, rec.State)
	}
}

// (2) Reconciler asymmetry: Op 2 (under-capacity) counts INGESTING
// from the DB, Op 3 (rotate at WindowEnd) walks INGESTING from the
// registry. When a shard_records row exists in INGESTING state but
// the in-memory registry doesn't have the entry, Op 2 sees IngestingCount
// already meeting target (no LaunchOp emitted) and Op 3 misses the
// row entirely (no RotateOp emitted). The shard sits — and the ASG
// holds a slot it can never fill.
func TestReconcile_DBRegistryAsymmetry_BlocksLaunch(t *testing.T) {
	now := time.Now()

	bucket := &AppBucket{
		ID:             1,
		Label:          "default",
		Apps:           []string{"weft"},
		Target:         2,
		Tier:           "small",
		WindowDuration: 7 * 24 * time.Hour,
	}

	// One real INGESTING shard in the registry. The zombie does NOT
	// appear here — that's the asymmetry. (In prod, this is what a
	// fresh coordinator boot looks like: LoadFromStore filtered the
	// zombie out, only the live shard's heartbeat re-populated the
	// registry.)
	entries := []*ShardEntry{
		{
			ID:          "shard-real",
			BucketID:    1,
			State:       StateIngesting,
			Apps:        []string{"weft"},
			WindowStart: now.Add(-time.Hour),
			WindowEnd:   now.Add(6 * 24 * time.Hour),
		},
	}

	// IngestingCount = 2 — one real (above) + one zombie (in DB only).
	// LaunchingCount = 0 — no in-flight LAUNCHING rows. This is what
	// snapshotForBucket would return in prod after the zombie row's
	// existence inflates the DB count.
	snap := ReconcilerSnapshot{
		Bucket:          bucket,
		Entries:         entries,
		LaunchingCount:  0,
		IngestingCount:  2,
		IsAsyncLauncher: true,
		Now:             now,
	}

	ops := reconcile(snap)

	// FAILING ASSERTION: target=2, only 1 real INGESTING shard, 0
	// LAUNCHING in flight. We want at least one LaunchShardOp so the
	// fleet recovers. Pre-fix: zero LaunchOps because the zombie
	// fakes the count.
	launchCount := 0
	for _, op := range ops {
		if _, ok := op.(LaunchShardOp); ok {
			launchCount++
		}
	}
	if launchCount == 0 {
		t.Errorf("registry has %d real INGESTING for target=%d, but "+
			"reconciler emitted 0 LaunchShardOp(s) because IngestingCount "+
			"counts a row the registry can't see — asymmetry between "+
			"DB-counted Op 2 and registry-walked Op 3", len(entries), bucket.Target)
	}
}

// (3) LoadFromStore reconstruct (per cmd/coordinator/main.go:261-262)
// returns an error for any non-ARCHIVED record, which causes the
// registry to skip it. After a coord restart, every live shard
// (INGESTING / READ_ONLY / LAUNCHING) is invisible to the registry
// until something — a heartbeat, a christen, an explicit lookup —
// fabricates an entry. A row that can't heartbeat (no instance) and
// has no other re-entry path stays invisible forever.
func TestLoadFromStore_DropsNonArchivedShards(t *testing.T) {
	reg := NewShardRegistry()
	fake := newFakeRegistryStore()
	reg.SetStore(fake)

	apps, _ := json.Marshal([]string{"weft"})
	now := time.Now().Format(time.RFC3339)

	// One row in each live state, plus an ARCHIVED control. In the
	// real prod DB, a coordinator restart needs to find every one of
	// these in the registry afterward — heartbeat-only re-entry is
	// not sufficient when an instance has died (wipe-and-await-rebind)
	// or never came up (LAUNCHING).
	for _, rec := range []store.ShardRecord{
		{ID: "shard-ingesting", State: string(StateIngesting), Apps: string(apps), WindowStart: now, WindowEnd: now, CreatedAt: now, BucketID: 1},
		{ID: "shard-readonly", State: string(StateReadOnly), Apps: string(apps), WindowStart: now, WindowEnd: now, CreatedAt: now, BucketID: 1},
		{ID: "shard-launching", State: "LAUNCHING", Apps: string(apps), WindowStart: now, WindowEnd: now, CreatedAt: now, BucketID: 1},
		{ID: "shard-archived", State: string(StateArchived), Apps: string(apps), WindowStart: now, WindowEnd: now, CreatedAt: now, BucketID: 1, ParquetPath: "s3://bucket/shard-archived/"},
	} {
		fake.records[rec.ID] = rec
	}

	// Reconstruct func mirrors cmd/coordinator/main.go's filter
	// exactly — only ARCHIVED rows with a parquet path produce a
	// shard.Shard, everything else returns an error and gets skipped.
	err := reg.LoadFromStore(func(rec store.ShardRecord) (shard.Shard, error) {
		if rec.State != string(StateArchived) || rec.ParquetPath == "" {
			return nil, fmt.Errorf("skip non-archived %s", rec.ID)
		}
		// Test stand-in for the cold-shard reconstruction. We don't
		// need a real S3 client — just a non-nil sentinel.
		return shard.NewDuckDBShard(rec.ID, "")
	})
	if err != nil {
		t.Fatal(err)
	}

	// FAILING ASSERTION: the registry should know about every live
	// shard the DB still has, plus the archived one. Pre-fix only
	// the ARCHIVED entry survives — the other three are silently
	// dropped because the reconstruct func errors on them.
	for _, want := range []string{"shard-ingesting", "shard-readonly", "shard-launching", "shard-archived"} {
		if reg.Get(want) == nil {
			t.Errorf("registry missing %q after LoadFromStore — live shards "+
				"are filtered out by the reconstruct func, leaving them "+
				"invisible to every code path that walks Entries", want)
		}
	}
}
