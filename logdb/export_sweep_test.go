//go:build test

package logdb

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/logtest"
)

// ── the orphan rule ─────────────────────────────────────────────────

func TestExportOrphanReason(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) time.Time { return now.Add(-age) }
	res := func(id string, age time.Duration) ExportResource {
		return ExportResource{Kind: "instance", ID: id, ShardID: "s-1", Attempt: 2, Created: at(age)}
	}
	archiving := &Shard{
		ShardRow: ShardRow{
			ID:     "s-1",
			State:  StateArchiving,
			Export: ExportFacts{Attempt: 2, Instance: "i-live", SnapshotID: "snap-live"},
		},
	}

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
		{"shard ARCHIVED", res("i-live", time.Hour), &Shard{
			ShardRow: ShardRow{
				ID:     "s-1",
				State:  StateArchived,
				Export: ExportFacts{Instance: "i-live"},
			},
		}, true},
		{"shard GONE", res("i-live", time.Hour), &Shard{
			ShardRow: ShardRow{
				ID:     "s-1",
				State:  StateGone,
				Export: ExportFacts{Instance: "i-live"},
			},
		}, true},
		{"never legitimately referenced, shard not archiving", res("i-x", time.Hour), &Shard{
			ShardRow: ShardRow{
				ID:    "s-1",
				State: StateReadOnly,
			},
		}, true},
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

func newTestSweeper(t *testing.T, lister *fakeExportLister) (*ArchiveSweeper, shardstore.Store, shardstore.Store) {
	t.Helper()
	_, db, _ := makeFixtureShardSM(t, StateArchiving) // seeds shard s-test and a bucket
	ss := db
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
	if n != 2 || len(lister.deleted) != 2 || !slices.Contains(lister.deleted, "i-old") || !slices.Contains(lister.deleted, "snap-gone") {
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
	if n != 1 || !slices.Contains(lister.deleted, "snap-b") {
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
	sw, ss, _ := newTestSweeper(t, nil)
	sw.Resources = nil
	now := sw.Clock()
	// The store's own StuckArchiving is held to the contract by its suite; here
	// it reports the shard as having been ARCHIVING for longer than the threshold.
	sw.Store = stuckStore{Store: ss, stuck: []StuckShard{{ShardID: "s-test", Since: now.Add(-archivingStuckAfter - time.Hour)}}}

	var runErr error
	out := captureTestLog(t, func() { runErr = sw.RunOnce(context.Background()) })
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(out, "ALERT archiving-stuck shard=s-test") {
		t.Fatalf("no stuck-archiving alert in the log:\n%s", out)
	}
}

// captureTestLog returns everything the std logger wrote while fn ran.
func captureTestLog(t *testing.T, fn func()) string {
	t.Helper()
	return logtest.Capture(fn)
}
