package store

import (
	"strconv"
	"strings"
	"testing"
)

// Migration 097 narrows an open incident's identity from (cron, condition,
// tag) to (cron, tag). Before it, a cron could hold several open incidents
// under different condition texts; its dedupe step must leave the newest of
// each (cron, tag) open and close the rest, so the narrower unique index can
// be built. The test replays that step against rows shaped like the old data.
func TestMigration097_KeepsNewestOpenIncidentPerCronAndTag(t *testing.T) {
	db, err := Open(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })

	raw, err := migrationsFS.ReadFile("migrations/097_alert_incident_deliveries.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "UPDATE public.alert_incidents")
	end := strings.Index(sql, "DROP INDEX")
	if start < 0 || end < start {
		t.Fatal("cannot find the dedupe statement in migration 097")
	}
	dedupe := sql[start:end]

	// Put the table back in its pre-097 shape: no narrow unique index, so
	// duplicates can exist.
	if err := tx.DB().Exec(`DROP INDEX public.idx_incident_watch_tag_open`).Error; err != nil {
		t.Fatal(err)
	}
	insert := func(id string, watch int, tag, opened string, closed bool) {
		t.Helper()
		closedAt := "NULL"
		if closed {
			closedAt = "'2026-01-01'"
		}
		q := `INSERT INTO public.alert_incidents (id, cron_watch_id, tag, opened_at, closed_at, last_seen_at)
		      VALUES ('` + id + `', ` + strconv.Itoa(watch) + `, '` + tag + `', '` + opened + `', ` + closedAt + `, '` + opened + `')`
		if err := tx.DB().Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	insert("old", 1, "", "2026-03-01", false)
	insert("new", 1, "", "2026-03-02", false)          // newest open for (1, "")
	insert("other-tag", 1, "cpu", "2026-03-01", false) // alone for (1, "cpu")
	insert("other-cron", 2, "", "2026-03-01", false)   // alone for (2, "")
	insert("already-closed", 1, "", "2026-03-03", true)

	if err := tx.DB().Exec(dedupe).Error; err != nil {
		t.Fatalf("dedupe: %v", err)
	}

	open := map[string]bool{}
	var ids []string
	if err := tx.DB().Raw(`SELECT id FROM public.alert_incidents WHERE closed_at IS NULL`).Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		open[id] = true
	}
	for id, want := range map[string]bool{"old": false, "new": true, "other-tag": true, "other-cron": true, "already-closed": false} {
		if open[id] != want {
			t.Errorf("incident %q open = %v, want %v", id, open[id], want)
		}
	}
}
