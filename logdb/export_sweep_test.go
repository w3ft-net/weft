//go:build test

package logdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
	"github.com/graybeardcorp/weft/store"
)

// ── ExportResources.ListExportResources / DeleteExportResource ──────

func TestExportResources_ListFindsOnlyExportResourcesWithTheirIdentity(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	ctx := context.Background()
	created := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	snapID, _ := x.CreateSnapshot(ctx, SnapshotSpec{ShardID: "s-1", Attempt: 2, VolumeID: "v"})
	api.snaps[0].started = created
	instID, _ := x.LaunchInstance(ctx, ExportSpec{ShardID: "s-1", Attempt: 2, SnapshotID: snapID, InstanceType: "r7g.large", AZ: "us-east-2a", Prefix: "s3://b/p"})
	api.insts[0].launched = created

	// Things that must not be listed: a shard's own instance and an already
	// terminated export instance.
	api.insts = append(api.insts, &fakeEC2Instance{id: "i-shard", tags: map[string]string{"shard_id": "s-1"}, state: "running"})
	api.insts = append(api.insts, &fakeEC2Instance{id: "i-dead", state: "terminated", launched: created,
		tags: map[string]string{"export_shard_id": "s-9", "export_attempt": "1"}})

	got, err := x.ListExportResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ExportResource{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if len(got) != 2 {
		t.Fatalf("listed %d resources, want the snapshot and the live instance only: %+v", len(got), got)
	}
	if r := byID[snapID]; r.Kind != "snapshot" || r.ShardID != "s-1" || r.Attempt != 2 || !r.Created.Equal(created) {
		t.Errorf("snapshot = %+v", r)
	}
	if r := byID[instID]; r.Kind != "instance" || r.ShardID != "s-1" || r.Attempt != 2 || !r.Created.Equal(created) {
		t.Errorf("instance = %+v", r)
	}
}

func TestExportResources_DeleteExportResource(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	ctx := context.Background()
	snapID, _ := x.CreateSnapshot(ctx, SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "v"})
	instID, _ := x.LaunchInstance(ctx, ExportSpec{ShardID: "s-1", Attempt: 1, SnapshotID: snapID, InstanceType: "r7g.large", AZ: "us-east-2a", Prefix: "s3://b/p"})

	if err := x.DeleteExportResource(ctx, ExportResource{Kind: "instance", ID: instID}); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteExportResource(ctx, ExportResource{Kind: "snapshot", ID: snapID}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminated) != 1 || api.terminated[0] != instID || len(api.deletedSnapshots) != 1 || api.deletedSnapshots[0] != snapID {
		t.Fatalf("terminated=%v deleted=%v", api.terminated, api.deletedSnapshots)
	}

	// Already gone is success: the sweep races the FSM's own cleanup.
	if err := x.DeleteExportResource(ctx, ExportResource{Kind: "instance", ID: "i-nope"}); err != nil {
		t.Errorf("deleting an unknown instance: %v", err)
	}
	if err := x.DeleteExportResource(ctx, ExportResource{Kind: "snapshot", ID: "snap-nope"}); err != nil {
		t.Errorf("deleting an unknown snapshot: %v", err)
	}
	if err := x.DeleteExportResource(ctx, ExportResource{Kind: "volume", ID: "vol-1"}); err == nil {
		t.Error("deleting an unknown kind succeeded")
	}
}

// ── the orphan rule ─────────────────────────────────────────────────

func TestExportOrphanReason(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) time.Time { return now.Add(-age) }
	res := func(id string, age time.Duration) ExportResource {
		return ExportResource{Kind: "instance", ID: id, ShardID: "s-1", Attempt: 2, Created: at(age)}
	}
	archiving := &Shard{ID: "s-1", State: StateArchiving, ExportAttempt: 2, ExportInstanceID: "i-live", ExportSnapshotID: "snap-live"}

	cases := []struct {
		name   string
		r      ExportResource
		sh     *Shard
		orphan bool
	}{
		{"referenced by the shard's current attempt", res("i-live", time.Hour), archiving, false},
		{"snapshot referenced by the current attempt", ExportResource{Kind: "snapshot", ID: "snap-live", ShardID: "s-1", Attempt: 2, Created: at(time.Hour)}, archiving, false},
		{"superseded attempt, past the grace", res("i-old", time.Hour), archiving, true},
		{"superseded attempt, within the grace", res("i-old", time.Minute), archiving, false},
		{"shard row gone, past the grace", res("i-live", time.Hour), nil, true},
		{"shard row gone, within the grace", res("i-live", time.Minute), nil, false},
		{"shard ARCHIVED", res("i-live", time.Hour), &Shard{ID: "s-1", State: StateArchived, ExportInstanceID: "i-live"}, true},
		{"shard GONE", res("i-live", time.Hour), &Shard{ID: "s-1", State: StateGone, ExportInstanceID: "i-live"}, true},
		{"never legitimately referenced, shard not archiving", res("i-x", time.Hour), &Shard{ID: "s-1", State: StateReadOnly}, true},
		{"older than the absolute cap, even though referenced", res("i-live", exportMaxResourceAge+time.Minute), archiving, true},
		{"unknown age is never swept on age alone", ExportResource{Kind: "instance", ID: "i-live", ShardID: "s-1", Attempt: 2}, archiving, false},
	}
	for _, c := range cases {
		reason := exportOrphanReason(c.r, c.sh, now)
		if (reason != "") != c.orphan {
			t.Errorf("%s: orphan reason = %q, want orphan=%v", c.name, reason, c.orphan)
		}
	}
}

// ── the sweep ───────────────────────────────────────────────────────

type fakeExportLister struct {
	mu        sync.Mutex
	resources []ExportResource
	listErr   error
	failIDs   map[string]error
	deleted   []string
}

func (f *fakeExportLister) ListExportResources(context.Context) ([]ExportResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resources, f.listErr
}

func (f *fakeExportLister) DeleteExportResource(_ context.Context, r ExportResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failIDs[r.ID]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, r.ID)
	return nil
}

func newTestSweeper(t *testing.T, lister *fakeExportLister) (*ArchiveSweeper, ShardStore, *store.Store) {
	t.Helper()
	_, db, _ := makeFixtureShardSM(t, StateArchiving) // seeds shard s-test and a bucket
	ss := ShardStore{db}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sw := &ArchiveSweeper{
		Resources: lister, Store: ss, Clock: func() time.Time { return now },
		IsLeader: func() bool { return true },
	}
	return sw, ss, db
}

func TestArchiveSweeper_DeletesOnlyOrphans(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	lister := &fakeExportLister{resources: []ExportResource{
		{Kind: "instance", ID: "i-live", ShardID: "s-test", Attempt: 1, Created: old},                           // referenced → keep
		{Kind: "instance", ID: "i-old", ShardID: "s-test", Attempt: 1, Created: old},                            // superseded → delete
		{Kind: "snapshot", ID: "snap-gone", ShardID: "s-vanished", Attempt: 1, Created: old},                    // row gone → delete
		{Kind: "snapshot", ID: "snap-young", ShardID: "s-vanished", Attempt: 1, Created: now.Add(-time.Minute)}, // young → keep
	}}
	sw, ss, _ := newTestSweeper(t, lister)
	if err := ss.SetExportInstance("s-test", "i-live"); err != nil {
		t.Fatal(err)
	}

	n, err := sw.SweepExportOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(lister.deleted) != 2 || !contains(lister.deleted, "i-old") || !contains(lister.deleted, "snap-gone") {
		t.Fatalf("deleted %d %v, want exactly i-old and snap-gone", n, lister.deleted)
	}
}

func TestArchiveSweeper_OneFailedDeleteDoesNotStopTheRest(t *testing.T) {
	old := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).Add(-time.Hour)
	lister := &fakeExportLister{
		resources: []ExportResource{
			{Kind: "snapshot", ID: "snap-a", ShardID: "s-vanished", Attempt: 1, Created: old},
			{Kind: "snapshot", ID: "snap-b", ShardID: "s-vanished", Attempt: 1, Created: old},
		},
		failIDs: map[string]error{"snap-a": errors.New("DeleteSnapshot throttled")},
	}
	sw, _, _ := newTestSweeper(t, lister)

	n, err := sw.SweepExportOnce(context.Background())
	if err == nil {
		t.Fatal("a failed delete should be reported")
	}
	if n != 1 || !contains(lister.deleted, "snap-b") {
		t.Fatalf("deleted %d %v, want snap-b still deleted", n, lister.deleted)
	}
}

func TestArchiveSweeper_OnlyTheLeaderSweeps(t *testing.T) {
	lister := &fakeExportLister{resources: []ExportResource{
		{Kind: "snapshot", ID: "snap-a", ShardID: "s-vanished", Attempt: 1, Created: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
	}}
	sw, _, _ := newTestSweeper(t, lister)
	sw.IsLeader = func() bool { return false }

	if err := sw.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lister.deleted) != 0 {
		t.Fatalf("a follower coordinator deleted %v", lister.deleted)
	}
}

func TestArchiveSweeper_NilResourcesStillAlertsOnStuckShards(t *testing.T) {
	sw, ss, db := newTestSweeper(t, nil)
	sw.Resources = nil
	now := sw.Clock()
	// The fixture's INSERT fired the row trigger, which stamped a real-time
	// ARCHIVING entry; clear it so only the backdated one counts.
	if err := db.DB().Exec(`DELETE FROM shard_operations WHERE shard_id = 's-test'`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB().Exec(`INSERT INTO shard_operations (shard_id, from_state, to_state, ts) VALUES ('s-test', 'READ_ONLY', 'ARCHIVING', ?)`,
		now.Add(-archivingStuckAfter-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	_ = ss

	var runErr error
	out := captureTestLog(t, func() { runErr = sw.RunOnce(context.Background()) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(out, "ALERT archiving-stuck shard=s-test") {
		t.Fatalf("no stuck-archiving alert in the log:\n%s", out)
	}
}

func TestShardStore_StuckArchiving(t *testing.T) {
	ss, db := exportFactsFixture(t) // s-test is ARCHIVING
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	insert := func(to string, ts time.Time) {
		t.Helper()
		if err := db.DB().Exec(`INSERT INTO shard_operations (shard_id, from_state, to_state, ts) VALUES ('s-test', 'X', ?, ?)`, to, ts).Error; err != nil {
			t.Fatal(err)
		}
	}

	insert("ARCHIVING", now.Add(-time.Hour))
	if got, err := ss.StuckArchiving(now.Add(-6 * time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("a shard archiving for 1h is not stuck at a 6h threshold: %+v %v", got, err)
	}

	insert("ARCHIVING", now.Add(-10*time.Hour)) // an earlier entry: only the latest counts
	if got, _ := ss.StuckArchiving(now.Add(-6 * time.Hour)); len(got) != 0 {
		t.Fatalf("an old ARCHIVING entry must not outweigh the latest: %+v", got)
	}

	// The shard re-entered ARCHIVING long ago and has been there since.
	if err := db.DB().Exec(`DELETE FROM shard_operations WHERE shard_id = 's-test'`).Error; err != nil {
		t.Fatal(err)
	}
	insert("ARCHIVING", now.Add(-10*time.Hour))
	got, err := ss.StuckArchiving(now.Add(-6 * time.Hour))
	if err != nil || len(got) != 1 || got[0].ShardID != "s-test" || got[0].Since.IsZero() {
		t.Fatalf("StuckArchiving = %+v %v, want s-test", got, err)
	}
}

// captureTestLog returns everything the std logger wrote while fn ran.
func captureTestLog(t *testing.T, fn func()) string {
	t.Helper()
	return weftlog.Capture(fn)
}
