package store

import (
	"os"
	"strings"
	"testing"
)

// Migration 091 rewrites the dashboard tile key watch: -> cron: in every
// saved body and every history version, without recording the rewrite as a
// new version.
func TestMigration091_DashboardCronKey(t *testing.T) {
	db, err := Open(TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })

	org, err := tx.FindOrCreateOrg("migration-091")
	if err != nil {
		t.Fatal(err)
	}
	const oldBlock = "# watch: not a key\ntitle: t\nsections:\n  - tiles:\n      - kind: counter\n        watch: errs\n      - watch: mem\n        kind: gauge\n"
	const oldFlow = "tiles:\n  - {kind: counter, watch: a}\n  - {watch: b, kind: gauge}\n"
	if err := tx.ExecForTest(`INSERT INTO dashboards (created_at, updated_at, name, yaml_body, org_id) VALUES (now(), now(), 'blk', ?, ?), (now(), now(), 'flow', ?, ?)`, "x: 1\n", org.ID, "x: 1\n", org.ID); err != nil {
		t.Fatal(err)
	}
	// Two updates each, so the history trigger captures old-key bodies as past versions.
	for _, u := range []struct{ name, first, second string }{{"blk", oldBlock, oldBlock + "# edited\n"}, {"flow", oldFlow, oldFlow + "# edited\n"}} {
		if err := tx.ExecForTest(`UPDATE dashboards SET yaml_body = ? WHERE name = ? AND org_id = ?`, u.first, u.name, org.ID); err != nil {
			t.Fatal(err)
		}
		if err := tx.ExecForTest(`UPDATE dashboards SET yaml_body = ? WHERE name = ? AND org_id = ?`, u.second, u.name, org.ID); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (versions int64) {
		tx.DB().Raw(`SELECT count(*) FROM dashboard_history WHERE org_id = ?`, org.ID).Scan(&versions)
		return
	}
	before := count()

	raw, err := os.ReadFile("migrations/091_dashboard_cron_key.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := string(raw)
	up = up[strings.Index(up, "-- +goose Up")+len("-- +goose Up") : strings.Index(up, "-- +goose Down")]
	for _, stmt := range strings.Split(up, ";\n") {
		if strings.TrimSpace(stmt) == "" || strings.HasPrefix(strings.TrimSpace(stmt), "--") && !strings.Contains(stmt, "\n") {
			continue
		}
		if err := tx.ExecForTest(stmt); err != nil {
			t.Fatalf("migration statement failed: %v\n%s", err, stmt)
		}
	}

	var bodies []string
	tx.DB().Raw(`SELECT yaml_body FROM dashboards WHERE org_id = ? UNION ALL SELECT yaml_body FROM dashboard_history WHERE org_id = ?`, org.ID, org.ID).Scan(&bodies)
	if len(bodies) < 4 {
		t.Fatalf("expected current and history rows, got %d", len(bodies))
	}
	for _, b := range bodies {
		// The real key moved; the comment mentioning `watch:` did not.
		if strings.Contains(strings.ReplaceAll(b, "# watch: not a key", ""), "watch:") {
			t.Errorf("a watch: key survived:\n%s", b)
		}
		if _, err := ParseDashboardYAML(b); err != nil && !strings.Contains(b, "x: 1") {
			t.Errorf("migrated body no longer parses: %v\n%s", err, b)
		}
	}
	if !strings.Contains(strings.Join(bodies, "\n"), "# watch: not a key") {
		t.Error("the comment was rewritten")
	}
	if after := count(); after != before {
		t.Errorf("migration recorded %d new history versions", after-before)
	}
}
