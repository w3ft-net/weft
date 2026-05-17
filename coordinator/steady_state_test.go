package coordinator

import (
	"context"
	"strconv"
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

	// noGap asserts the core invariant the gate exists to enforce:
	// once bootstrapped the bucket must never drop below Target
	// INGESTING shards — a window roll never opens a zero-ingest
	// write-gap.
	noGap := func(label string) {
		t.Helper()
		if ing, _, _ := countByState(); ing < bucket.Target {
			t.Fatalf("%s: INGESTING=%d < Target=%d — write-gap (state machine violated)", label, ing, bucket.Target)
		}
	}

	// prewarm models the production pre-warm path (always on): before
	// the current window closes, Target next-window shards are brought
	// up and reach INGESTING. We launch real shards via LaunchShardOp
	// (so they're archivable later) then stamp them onto the simulated
	// next window. The sync launcher assigns real-clock windows (far
	// future vs the simulated 2026-05-05 clock), so the just-launched
	// replacements are exactly the INGESTING shards whose WindowStart
	// is After the simulated window end — aged shards keep their sim
	// windows and stay rotate-eligible.
	prewarm := func(ws, we time.Time) {
		for i := 0; i < bucket.Target; i++ {
			if err := (LaunchShardOp{Bucket: bucket}).Apply(context.Background(), prov); err != nil {
				t.Fatalf("prewarm launch: %v", err)
			}
		}
		for _, e := range reg.Entries() {
			if e.State == StateIngesting && e.WindowStart.After(we) {
				e.WindowStart = ws
				e.WindowEnd = we
			}
		}
	}

	// ── Hour 0: bootstrap = pre-warm the first window ──
	w0s, w0e := simNow, simNow.Add(time.Hour)
	prewarm(w0s, w0e)
	tick(simNow)
	noGap("hour 0")
	if ing, ro, arch := countByState(); ing != 2 || ro != 0 || arch != 0 {
		t.Fatalf("hour 0: want 2/0/0 (I/RO/A), got %d/%d/%d", ing, ro, arch)
	}

	// Each subsequent hour: pre-warm the NEXT window (replacements
	// reach INGESTING while the current shards are still hot — no
	// gap), then the clock crosses WindowEnd and a single tick lets
	// the gate rotate the now-old shards (Target still satisfied by
	// the pre-warmed replacements) and archive any ripe READ_ONLY.
	curEnd := w0e
	wantArch := 0
	for hour := 1; hour <= 3; hour++ {
		label := "hour " + strconv.Itoa(hour)
		nextStart, nextEnd := curEnd, curEnd.Add(time.Hour)
		prewarm(nextStart, nextEnd) // replacements INGESTING before the roll
		noGap(label + " prewarm")

		simNow = simNow.Add(61 * time.Minute) // cross curEnd
		tick(simNow)                          // gate releases: rotate old, archive ripe RO
		noGap(label + " tick")

		if hour >= 2 {
			wantArch += 2 // the window two cycles back ages past retention
		}
		ing, ro, arch := countByState()
		if ing != 2 || ro != 2 || arch != wantArch {
			t.Fatalf("%s: want 2/2/%d (I/RO/A), got %d/%d/%d", label, wantArch, ing, ro, arch)
		}
		assertNoWindowOverlap(t, reg, label)
		curEnd = nextEnd
	}
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
// future. The reconciler detects the stale (over-wide) span and
// retires them — but capacity-gated: it must NOT rotate both legacy
// shards while the bucket would be left with zero ingest capacity.
// They keep INGESTING (no write-gap) until pre-warmed replacements
// reach INGESTING, then they rotate.
func TestSteadyState_LegacyWideWindowShards(t *testing.T) {
	reg := NewShardRegistry()

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

	countOps := func(ops []ReconcileOp) (rotate, launch int) {
		for _, op := range ops {
			switch op.(type) {
			case RotateToReadOnlyOp:
				rotate++
			case LaunchShardOp:
				launch++
			}
		}
		return
	}

	// Phase 1 — no replacements yet. The stale span is detected, but
	// rotating either legacy shard would drop the bucket below
	// Target=2 INGESTING. The gate blocks rotation entirely (the
	// legacy shards keep ingesting — no write-gap). Capacity is met
	// by the two still-INGESTING legacy shards, so Op 2 does not
	// reactively launch; pre-warm (always on, modelled in
	// TestSteadyState_LifecycleConverges) is what brings the
	// replacements up.
	snap := ReconcilerSnapshot{Bucket: bucket, Entries: reg.Entries(), Now: now}
	rotate, launch := countOps(reconcile(snap))
	if rotate != 0 {
		t.Errorf("phase 1: want 0 rotations (would open a write-gap), got %d", rotate)
	}
	if launch != 0 {
		t.Errorf("phase 1: want 0 LaunchShardOps (capacity met by legacy shards still ingesting), got %d", launch)
	}

	// Phase 2 — replacements have launched and reached INGESTING with
	// a valid current window. Now the legacy shards may rotate without
	// dropping below Target.
	for _, id := range []string{"shard-new-a", "shard-new-b"} {
		reg.Add(&ShardEntry{
			ID:          id,
			BucketID:    1,
			State:       StateIngesting,
			WindowStart: now.Add(-1 * time.Minute),
			WindowEnd:   now.Add(59 * time.Minute),
			Shard:       &failingShard{id: id},
		})
	}
	snap = ReconcilerSnapshot{Bucket: bucket, Entries: reg.Entries(), Now: now}
	rotate, _ = countOps(reconcile(snap))
	if rotate != 2 {
		t.Errorf("phase 2: want 2 RotateToReadOnlyOps once replacements INGESTING, got %d: %v",
			rotate, describeOps(reconcile(snap)))
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
