package store

import (
	"testing"
	"time"
)

type versionRow struct {
	VersionID  int64
	AuthoredAt time.Time
	AuthoredBy *string
}

func liveBinVersion(t *testing.T, db *Store, org OrgID, name string) versionRow {
	t.Helper()
	var r versionRow
	err := db.db.Raw(`SELECT version_id, authored_at, authored_by_user_id AS authored_by
		FROM shell_functions WHERE org_id = ? AND name = ?`, org, name).Scan(&r).Error
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVersionIdentity_BinTravelsIntoHistory(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vid-bin")
	alice, _ := db.CreateUser("alice-vid-bin@example.test")
	bob, _ := db.CreateUser("bob-vid-bin@example.test")

	if err := db.SaveShellFunction(cust.ID, alice.ID, "x", "echo 1", "bash"); err != nil {
		t.Fatal(err)
	}
	v1 := liveBinVersion(t, db, cust.ID, "x")
	if v1.VersionID == 0 || v1.AuthoredBy == nil || *v1.AuthoredBy != alice.ID {
		t.Fatalf("v1 = %+v, want a version_id authored by alice", v1)
	}

	// An identical re-save is not a new version.
	_ = db.SaveShellFunction(cust.ID, alice.ID, "x", "echo 1", "bash")
	if got := liveBinVersion(t, db, cust.ID, "x"); got.VersionID != v1.VersionID {
		t.Errorf("no-op save changed version_id %d -> %d", v1.VersionID, got.VersionID)
	}

	// Bob overwrites: v1 moves to history *keeping its identity*, HEAD is new.
	if err := db.SaveShellFunction(cust.ID, bob.ID, "x", "echo 2", "bash"); err != nil {
		t.Fatal(err)
	}
	v2 := liveBinVersion(t, db, cust.ID, "x")
	if v2.VersionID == v1.VersionID || v2.AuthoredBy == nil || *v2.AuthoredBy != bob.ID {
		t.Fatalf("v2 = %+v, want a new version_id authored by bob", v2)
	}
	if !v2.AuthoredAt.After(v1.AuthoredAt) {
		t.Errorf("v2.authored_at %v not after v1 %v", v2.AuthoredAt, v1.AuthoredAt)
	}
	var h versionRow
	if err := db.db.Raw(`SELECT h.version_id, h.authored_at, h.authored_by_user_id AS authored_by
		FROM shell_function_history h JOIN shell_functions f ON f.id = h.function_id
		WHERE f.org_id = ? AND f.name = 'x'`, cust.ID).Scan(&h).Error; err != nil {
		t.Fatal(err)
	}
	if h.VersionID != v1.VersionID || !h.AuthoredAt.Equal(v1.AuthoredAt) ||
		h.AuthoredBy == nil || *h.AuthoredBy != alice.ID {
		t.Errorf("history row = %+v, want v1's identity %+v", h, v1)
	}
}

func TestVersionIdentity_CronTravelsIntoHistory(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("vid-cron")
	if err := db.SaveCronWatch(cust.ID, "", "t", "*/10 * * * *", "echo a", nil); err != nil {
		t.Fatal(err)
	}
	var v1 versionRow
	q := `SELECT version_id, authored_at FROM cron_watches WHERE org_id = ? AND name = 't'`
	db.db.Raw(q, cust.ID).Scan(&v1)
	if err := db.SaveCronWatch(cust.ID, "", "t", "*/10 * * * *", "echo b", nil); err != nil {
		t.Fatal(err)
	}
	var v2, h versionRow
	db.db.Raw(q, cust.ID).Scan(&v2)
	db.db.Raw(`SELECT h.version_id, h.authored_at FROM cron_watch_history h
		JOIN cron_watches w ON w.id = h.watch_id WHERE w.org_id = ? AND w.name = 't'`, cust.ID).Scan(&h)
	if v1.VersionID == 0 || v2.VersionID == v1.VersionID {
		t.Fatalf("v1=%+v v2=%+v, want distinct non-zero version ids", v1, v2)
	}
	if h.VersionID != v1.VersionID || !h.AuthoredAt.Equal(v1.AuthoredAt) {
		t.Errorf("history = %+v, want v1 identity %+v", h, v1)
	}
}
