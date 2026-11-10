package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Migration 113 retires the org-wide cronmgr bit: whoever held it gets write
// on the namespaces they can read, and nobody else gains anything.
func TestMigration113_CronmgrBecomesWriteOnReadableNamespaces(t *testing.T) {
	db := teamPermsStore(t)
	pid := os.Getpid()
	org, _ := db.FindOrCreateOrg(fmt.Sprintf("mig113-%d", pid))
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	beta, _ := db.FindOrCreateNamespace(org.ID, "beta")
	mk := func(name string, cronmgr bool) string {
		u := addTeamUser(t, db, org.ID, fmt.Sprintf("%s-%d@mig113.test", name, pid), "member")
		if cronmgr {
			if err := db.setOrgBit(org.ID, u.ID, "can_manage_watches", true); err != nil {
				t.Fatal(err)
			}
		}
		return u.ID
	}
	manager, reader, blind := mk("manager", true), mk("reader", false), mk("blind", true)
	for _, g := range []struct {
		user string
		ns   uint
	}{{manager, alpha.ID}, {reader, alpha.ID}, {reader, beta.ID}} {
		if err := db.UpsertNamespacePermission(&NamespacePermission{UserID: g.user, NamespaceID: g.ns, CanRead: true}); err != nil {
			t.Fatal(err)
		}
	}

	sqlText, err := migrationsFS.ReadFile("migrations/113_namespace_write.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(strings.SplitN(string(sqlText), "-- +goose Down", 2)[0], "UPDATE", 2)[1]
	if err := db.DB().Exec("UPDATE" + up).Error; err != nil {
		t.Fatalf("backfill: %v", err)
	}

	can := func(user string, ns uint) bool {
		p, err := db.GetNamespacePermission(user, ns)
		return err == nil && p.CanWrite
	}
	if !can(manager, alpha.ID) {
		t.Error("a cronmgr who reads alpha should write it")
	}
	if can(reader, alpha.ID) || can(reader, beta.ID) {
		t.Error("a reader without cronmgr gained write")
	}
	if _, err := db.GetNamespacePermission(blind, alpha.ID); err == nil {
		t.Error("a cronmgr with no read on alpha gained a row")
	}
}
