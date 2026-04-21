package coordinator

import (
	"context"
	"testing"
	"time"
)

// bucketStub returns a stubBuckets whose default bucket has the
// given target and retention. Window-rotation and archive tests
// usually pass target=0 or 1 (no scale-up) with a specific
// retention; under-capacity tests do the inverse.
func bucketStub(target int, retention time.Duration) *stubBuckets {
	return &stubBuckets{bucket: &AppBucket{
		ID: defaultBucketID, Label: "default", Apps: nil,
		Target: target, RetentionPeriod: retention, Tier: "small",
	}}
}

// A shard whose WindowEnd has passed must transition INGESTING →
// READ_ONLY on the next tick — and the transition must be
// persisted, so a coordinator restart doesn't see it as still
// INGESTING.
func TestCheck_WindowEndPastNow_RotatesToReadOnly(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	now := time.Now().UTC()
	reg.Add(&ShardEntry{
		ID:          "shard-past-window",
		BucketID:    defaultBucketID,
		State:       StateIngesting,
		Shard:       &failingShard{id: "shard-past-window"},
		WindowStart: now.Add(-2 * time.Hour),
		WindowEnd:   now.Add(-1 * time.Minute), // just past
		CreatedAt:   now.Add(-2 * time.Hour),
		EBSVolumeID: "vol-x",
	})

	prov := &Provisioner{
		registry: reg,
		launcher: &stubLauncher{addr: "10.0.0.99:9000"},
		config: ProvisionerConfig{
			WindowDuration:   time.Hour,
			ConfigureRetries: 1,
			ConfigureBackoff: time.Millisecond,
		},
		stats:      NewAppStats(),
		Buckets:    bucketStub(1, 0),
		configurer: &failingConfigurer{}, // no replacement gRPC; we're asserting the transition
	}
	prov.check(context.Background())

	got := reg.Get("shard-past-window")
	if got == nil || got.State != StateReadOnly {
		t.Fatalf("expected READ_ONLY, got entry=%+v", got)
	}
	rec, ok := fake.get("shard-past-window")
	if !ok {
		t.Fatal("store missing shard-past-window")
	}
	if rec.State != string(StateReadOnly) {
		t.Errorf("store state: got %q want READ_ONLY", rec.State)
	}
}

// A shard well inside its window must NOT rotate. Guards against a
// too-aggressive check that rotates on any non-zero WindowEnd.
func TestCheck_WindowEndInFuture_NoRotation(t *testing.T) {
	reg := NewShardRegistry()

	now := time.Now().UTC()
	reg.Add(&ShardEntry{
		ID:          "shard-fresh-window",
		BucketID:    defaultBucketID,
		State:       StateIngesting,
		Shard:       &failingShard{id: "shard-fresh-window"},
		WindowStart: now.Add(-10 * time.Minute),
		WindowEnd:   now.Add(50 * time.Minute),
		CreatedAt:   now.Add(-10 * time.Minute),
	})

	prov := &Provisioner{
		registry: reg,
		launcher: &stubLauncher{},
		config: ProvisionerConfig{
			WindowDuration: time.Hour,
		},
		stats:      NewAppStats(),
		Buckets:    bucketStub(1, 0),
		configurer: &failingConfigurer{},
	}
	prov.check(context.Background())

	got := reg.Get("shard-fresh-window")
	if got == nil || got.State != StateIngesting {
		t.Fatalf("expected INGESTING, got %+v", got)
	}
}

// A READ_ONLY shard whose WindowEnd is more than RetentionPeriod in
// the past must be archived on the next tick.
func TestCheck_ReadOnlyPastRetention_TriggersArchive(t *testing.T) {
	vm := &retiringVolumeManager{}
	reg := NewShardRegistry()

	now := time.Now().UTC()
	entry := testArchiveIngestedShard(t, "shard-retire-candidate", "vol-retire")
	entry.BucketID = defaultBucketID
	entry.State = StateReadOnly
	entry.WindowStart = now.Add(-3 * time.Hour)
	entry.WindowEnd = now.Add(-2 * time.Hour) // past
	entry.CreatedAt = now.Add(-3 * time.Hour)
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			WindowDuration: time.Hour,
			ParquetDir:     t.TempDir(),
		},
		stats:   NewAppStats(),
		Buckets: bucketStub(0, time.Hour), // retention 1h: 2h past WindowEnd → eligible
		volumes: vm,
	}
	prov.check(context.Background())

	got := reg.Get("shard-retire-candidate")
	if got == nil {
		t.Fatal("archived entry missing from registry")
	}
	if got.State != StateArchived {
		t.Errorf("state: got %s want ARCHIVED", got.State)
	}
	if len(vm.snapshotCalls) != 1 {
		t.Errorf("expected 1 SnapshotVolume call as part of retirement, got %d", len(vm.snapshotCalls))
	}
}

// A READ_ONLY shard still inside retention must NOT archive.
func TestCheck_ReadOnlyWithinRetention_NoArchive(t *testing.T) {
	vm := &retiringVolumeManager{}
	reg := NewShardRegistry()

	now := time.Now().UTC()
	entry := testArchiveIngestedShard(t, "shard-still-warm", "vol-warm")
	entry.BucketID = defaultBucketID
	entry.State = StateReadOnly
	entry.WindowStart = now.Add(-90 * time.Minute)
	entry.WindowEnd = now.Add(-30 * time.Minute) // 30m past, but retention is 2h
	entry.CreatedAt = now.Add(-90 * time.Minute)
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			WindowDuration: time.Hour,
			ParquetDir:     t.TempDir(),
		},
		stats:   NewAppStats(),
		Buckets: bucketStub(0, 2*time.Hour),
		volumes: vm,
	}
	prov.check(context.Background())

	got := reg.Get("shard-still-warm")
	if got == nil || got.State != StateReadOnly {
		t.Fatalf("expected still READ_ONLY, got %+v", got)
	}
	if len(vm.snapshotCalls) != 0 {
		t.Errorf("expected no snapshot yet, got %d", len(vm.snapshotCalls))
	}
}

// Retention must be measured from WindowEnd, not CreatedAt. A
// long-lived shard that only recently transitioned to READ_ONLY
// should be protected by retention even if its CreatedAt is old.
func TestCheck_OldCreatedAt_RecentWindowEnd_NoArchive(t *testing.T) {
	vm := &retiringVolumeManager{}
	reg := NewShardRegistry()

	now := time.Now().UTC()
	entry := testArchiveIngestedShard(t, "shard-aged-but-recent-roll", "vol-ok")
	entry.BucketID = defaultBucketID
	entry.State = StateReadOnly
	// Created long ago — existed as INGESTING for 10h — but only
	// became READ_ONLY 30m ago. Retention is 1h so we still want
	// this shard hot.
	entry.CreatedAt = now.Add(-10 * time.Hour)
	entry.WindowStart = now.Add(-10 * time.Hour)
	entry.WindowEnd = now.Add(-30 * time.Minute)
	reg.Add(entry)

	prov := &Provisioner{
		registry: reg,
		launcher: &recordingLauncher{},
		config: ProvisionerConfig{
			WindowDuration: time.Hour,
			ParquetDir:     t.TempDir(),
		},
		stats:   NewAppStats(),
		Buckets: bucketStub(0, time.Hour),
		volumes: vm,
	}
	prov.check(context.Background())

	got := reg.Get("shard-aged-but-recent-roll")
	if got == nil || got.State != StateReadOnly {
		t.Fatalf("shard within retention-past-WindowEnd must stay READ_ONLY, got %+v", got)
	}
	if len(vm.snapshotCalls) != 0 {
		t.Errorf("no archive should have been attempted, got %d snapshot calls", len(vm.snapshotCalls))
	}
}

// Zero-window shards (legacy adoption case) must not match the
// wall-clock rotation check. Without this guard we'd immediately
// rotate every heartbeat-adopted shard on first tick.
func TestCheck_ZeroWindow_NoRotation(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{
		ID:       "shard-no-window",
		BucketID: defaultBucketID,
		State:    StateIngesting,
		Shard:    &failingShard{id: "shard-no-window"},
		// WindowStart / WindowEnd zero value
		CreatedAt: time.Now().UTC(),
	})

	prov := &Provisioner{
		registry: reg,
		launcher: &stubLauncher{},
		config: ProvisionerConfig{
			WindowDuration: time.Hour,
		},
		stats:      NewAppStats(),
		Buckets:    bucketStub(1, 0),
		configurer: &failingConfigurer{},
	}
	prov.check(context.Background())

	got := reg.Get("shard-no-window")
	if got == nil || got.State != StateIngesting {
		t.Fatalf("zero-window shard must not auto-rotate, got %+v", got)
	}
}
