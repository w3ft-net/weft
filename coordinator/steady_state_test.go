package coordinator

import (
	"context"
	"testing"
	"time"
)

// TestSteadyState_LifecycleConverges simulates multiple hours of
// reconciler ticks for a bucket with target=2 and 1h windows. After
// enough ticks the registry must converge to exactly:
//
//	2 INGESTING  — current window
//	2 READ_ONLY  — previous window (within retention)
//	2×(N-2) ARCHIVED — all earlier windows
//
// No overlapping INGESTING and READ_ONLY windows within the bucket.
// This is the invariant production violated: zero READ_ONLY shards
// and multiple stuck INGESTING shards across different windows.
func TestSteadyState_LifecycleConverges(t *testing.T) {
	bucket := &AppBucket{
		ID:             1,
		Label:          "default",
		Apps:           []string{"weft"},
		Target:         2,
		Tier:           "small",
		WindowDuration: time.Hour,
	}

	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	launcher := newLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	prov := NewProvisioner(reg, launcher, ProvisionerConfig{
		WindowDuration: time.Hour,
		ParquetDir:     t.TempDir(),
	})
	prov.Buckets = &stubBuckets{bucket: bucket}

	// Start at a clean hour boundary so windows are predictable.
	simNow := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)

	// tick runs one reconciler pass at the given simulated time.
	// reconcile() is pure: it reads the snapshot and returns ops.
	// Each op is then applied against the live provisioner.
	tick := func(at time.Time) []string {
		snap := ReconcilerSnapshot{
			Bucket:          bucket,
			Now:             at,
			IsAsyncLauncher: false,
		}
		for _, e := range reg.Entries() {
			if e.BucketID == bucket.ID {
				snap.Entries = append(snap.Entries, e)
			}
		}
		for _, e := range snap.Entries {
			if e.State == StateIngesting {
				snap.IngestingCount++
			}
		}
		ops := reconcile(snap)
		var descs []string
		for _, op := range ops {
			descs = append(descs, op.Describe())
			if err := op.Apply(context.Background(), prov); err != nil {
				t.Logf("op %s: %v", op.Describe(), err)
			}
		}
		return descs
	}

	// stampWindow overwrites every INGESTING shard's window to match
	// the simulated clock. The sync launcher uses time.Now()
	// internally, so launched shards carry real-clock windows that
	// won't match the simulated hour boundaries.
	stampWindow := func(ws, we time.Time) {
		for _, e := range reg.Entries() {
			if e.State == StateIngesting {
				if e.WindowStart.Equal(ws) && e.WindowEnd.Equal(we) {
					continue
				}
				e.WindowStart = ws
				e.WindowEnd = we
			}
		}
	}

	countByState := func() (ingesting, readOnly, archived int) {
		for _, e := range reg.Entries() {
			switch e.State {
			case StateIngesting:
				ingesting++
			case StateReadOnly:
				readOnly++
			case StateArchived:
				archived++
			}
		}
		return
	}

	// ── Hour 0 (T+0): empty registry → launch 2 INGESTING ──

	tick(simNow)
	w0s, w0e := simNow, simNow.Add(time.Hour)
	stampWindow(w0s, w0e)

	ing, ro, arch := countByState()
	if ing != 2 || ro != 0 || arch != 0 {
		t.Fatalf("hour 0: want 2/0/0 (I/RO/A), got %d/%d/%d", ing, ro, arch)
	}

	// ── Hour 1 (T+61m): window closed ──
	// Tick 1a: rotate 2 INGESTING → READ_ONLY (Op 3 fires because
	// simNow > WindowEnd). Op 2 still sees 2 INGESTING in the
	// frozen snapshot, so no launches yet.
	simNow = simNow.Add(61 * time.Minute)
	tick(simNow)

	ing, ro, arch = countByState()
	if ro != 2 {
		t.Fatalf("hour 1a: want 2 READ_ONLY after rotation, got I=%d RO=%d A=%d", ing, ro, arch)
	}

	// Tick 1b: fresh snapshot sees 0 INGESTING → launches 2.
	tick(simNow)
	w1s, w1e := w0e, w0e.Add(time.Hour)
	stampWindow(w1s, w1e)

	ing, ro, arch = countByState()
	if ing != 2 || ro != 2 || arch != 0 {
		t.Fatalf("hour 1b: want 2/2/0 (I/RO/A), got %d/%d/%d", ing, ro, arch)
	}
	assertNoWindowOverlap(t, reg, "hour 1")

	// ── Hour 2 (T+122m): archive old READ_ONLY + rotate current ──
	simNow = simNow.Add(61 * time.Minute)
	tick(simNow) // rotate + archive
	tick(simNow) // launch replacements
	w2s, w2e := w1e, w1e.Add(time.Hour)
	stampWindow(w2s, w2e)

	ing, ro, arch = countByState()
	if ing != 2 || ro != 2 || arch != 2 {
		t.Fatalf("hour 2: want 2/2/2 (I/RO/A), got %d/%d/%d", ing, ro, arch)
	}
	assertNoWindowOverlap(t, reg, "hour 2")

	// ── Hour 3 (T+183m): another cycle ──
	simNow = simNow.Add(61 * time.Minute)
	tick(simNow)
	tick(simNow)
	stampWindow(w2e, w2e.Add(time.Hour))

	ing, ro, arch = countByState()
	if ing != 2 || ro != 2 || arch != 4 {
		t.Fatalf("hour 3: want 2/2/4 (I/RO/A), got %d/%d/%d", ing, ro, arch)
	}
	assertNoWindowOverlap(t, reg, "hour 3")
}

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

	reg.Add(&ShardEntry{
		ID:          "shard-legacy",
		BucketID:    1,
		State:       StateIngesting,
		WindowStart: now.Add(-time.Hour),
		WindowEnd:   futureEnd,
		Shard:       &failingShard{id: "shard-legacy"},
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
	rec.WindowEnd = closedEnd.Format(time.RFC3339)
	fake.mu.Lock()
	fake.records["shard-legacy"] = rec
	fake.mu.Unlock()

	// Before refresh: in-memory still has old window.
	if got := reg.Get("shard-legacy").WindowEnd; got.Equal(closedEnd) {
		t.Fatal("in-memory entry should NOT reflect DB edit before refresh")
	}

	reg.RefreshFromStore()

	// After refresh: in-memory matches the DB edit.
	if got := reg.Get("shard-legacy").WindowEnd; !got.Equal(closedEnd) {
		t.Fatalf("post-refresh WindowEnd: got %v want %v", got, closedEnd)
	}

	// The reconciler should now rotate this shard since WindowEnd < now.
	bucket := &AppBucket{ID: 1, Label: "default", Target: 0, Tier: "small", WindowDuration: time.Hour}
	snap := ReconcilerSnapshot{
		Bucket:  bucket,
		Entries: []*ShardEntry{reg.Get("shard-legacy")},
		Now:     now,
	}
	ops := reconcile(snap)
	rotated := false
	for _, op := range ops {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			rotated = true
		}
	}
	if !rotated {
		t.Errorf("expected RotateToReadOnlyOp after refresh brought WindowEnd into the past; ops=%v", describeOps(ops))
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
	reg.Add(&ShardEntry{
		ID:       "shard-x",
		BucketID: 1,
		State:    StateIngesting,
		Shard:    sh,
	})

	// Mutate a metadata field in the DB.
	rec, _, _ := fake.GetShard("shard-x")
	rec.Tier = "large"
	fake.mu.Lock()
	fake.records["shard-x"] = rec
	fake.mu.Unlock()

	reg.RefreshFromStore()

	entry := reg.Get("shard-x")
	if entry.Tier != "large" {
		t.Errorf("Tier not refreshed: got %q want large", entry.Tier)
	}
	if entry.Shard != sh {
		t.Error("Shard pointer was clobbered by RefreshFromStore")
	}
}

// TestSteadyState_LegacyWideWindowShards reproduces the production
// bug: the bucket's WindowDuration was changed from 7d to 1h, but
// shards launched under the old duration have window_end far in the
// future. An operator edits window_end in the DB to force rotation.
// With RefreshFromStore, the next tick picks up the edit and rotates
// them — without it, the in-memory WindowEnd stays stale and the
// shards sit in INGESTING forever.
func TestSteadyState_LegacyWideWindowShards(t *testing.T) {
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)

	now := time.Now().UTC().Truncate(time.Second)
	bucket := &AppBucket{
		ID: 1, Label: "default", Apps: []string{"weft"},
		Target: 2, Tier: "small", WindowDuration: time.Hour,
	}

	// Two legacy shards with 7-day windows — exactly what production had.
	for _, id := range []string{"shard-legacy-a", "shard-legacy-b"} {
		reg.Add(&ShardEntry{
			ID:          id,
			BucketID:    1,
			State:       StateIngesting,
			WindowStart: now.Add(-24 * time.Hour),
			WindowEnd:   now.Add(6 * 24 * time.Hour), // 7d window, ends in 6 days
			Shard:       &failingShard{id: id},
		})
	}

	// Reconciler tick: WindowEnd is in the future → no rotation.
	snap := ReconcilerSnapshot{
		Bucket:  bucket,
		Entries: reg.Entries(),
		Now:     now,
	}
	ops := reconcile(snap)
	for _, op := range ops {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			t.Fatal("legacy shards should NOT rotate before DB edit")
		}
	}

	// Operator edits both rows: window_end = now - 1m.
	closedEnd := now.Add(-time.Minute)
	for _, id := range []string{"shard-legacy-a", "shard-legacy-b"} {
		rec, ok := fake.get(id)
		if !ok {
			t.Fatalf("%s missing from store", id)
		}
		rec.WindowEnd = closedEnd.Format(time.RFC3339)
		fake.mu.Lock()
		fake.records[id] = rec
		fake.mu.Unlock()
	}

	// Refresh picks up the edit.
	reg.RefreshFromStore()

	for _, id := range []string{"shard-legacy-a", "shard-legacy-b"} {
		if got := reg.Get(id).WindowEnd; !got.Equal(closedEnd) {
			t.Fatalf("%s WindowEnd after refresh: got %v want %v", id, got, closedEnd)
		}
	}

	// Next reconciler tick: both shards rotate.
	snap = ReconcilerSnapshot{
		Bucket:  bucket,
		Entries: reg.Entries(),
		Now:     now,
	}
	ops = reconcile(snap)
	rotated := 0
	for _, op := range ops {
		if _, ok := op.(RotateToReadOnlyOp); ok {
			rotated++
		}
	}
	if rotated != 2 {
		t.Errorf("want 2 RotateToReadOnlyOps after DB edit + refresh, got %d: %v",
			rotated, describeOps(ops))
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
