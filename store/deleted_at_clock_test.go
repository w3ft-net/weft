package store

import (
	"testing"
	"time"
)

// Version history orders a deletion by deleted_at and every save by
// authored_at. authored_at is the database's clock, so deleted_at must be
// too: stamped by the app instead, a deletion sorts before saves that ran
// earlier whenever the app's clock is behind the database's, and `git log`
// shows the delete in the wrong place. Here the app clock is an hour behind.
func TestDeletionsUseTheDatabaseClock(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("deleted-at-clock")
	cfg := db.DB().Config
	appNow := cfg.NowFunc
	cfg.NowFunc = func() time.Time { return time.Now().Add(-time.Hour) }
	t.Cleanup(func() { cfg.NowFunc = appNow })

	if err := db.SaveCronWatch(org.ID, 0, "", "errs", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(org.ID, "", "count", "echo 1", "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDashboard(org.ID, "", "prod", "tiles:\n  - kind: counter\n    cron: errs\n"); err != nil {
		t.Fatal(err)
	}
	// Saved after the others, so the deletions below must sort after it.
	if err := db.SaveDashboard(org.ID, "", "later", "tiles:\n  - kind: table\n    cron: errs\n"); err != nil {
		t.Fatal(err)
	}
	for _, del := range []func() error{
		func() error { return db.DeleteDashboard(org.ID, "prod") },
		func() error { return db.DeleteShellFunction(org.ID, "count") },
		func() error { return db.DeleteCronWatch(org.ID, 0, "errs") },
	} {
		if err := del(); err != nil {
			t.Fatal(err)
		}
	}

	versions, err := db.ListObjectVersions(org.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lastSave time.Time
	for _, v := range versions {
		if v.Name == "later.yaml" {
			lastSave = v.AuthoredAt
		}
	}
	deleted := 0
	for _, v := range versions {
		if v.DeletedAt == nil {
			continue
		}
		deleted++
		if v.DeletedAt.Before(lastSave) {
			t.Errorf("%s/%s deleted at %s, before a save at %s that ran first",
				v.Kind, v.Name, v.DeletedAt.Format(time.RFC3339Nano), lastSave.Format(time.RFC3339Nano))
		}
	}
	if deleted != 3 {
		t.Fatalf("%d deleted versions, want 3", deleted)
	}
}
