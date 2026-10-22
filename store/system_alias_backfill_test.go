package store

import (
	"testing"
)

// EnsureSystemAliases gives every org that has an owner but no `system` alias
// one, naming the owner's address, and leaves everything else alone. It runs
// at migrate time, so orgs that predate the rule are brought in line.
func TestEnsureSystemAliases_BackfillsOrgsThatPredateTheRule(t *testing.T) {
	db := testOpen(t)

	fresh, _ := db.FindOrCreateOrg("backfill-none")
	freshOwner := addTeamUser(t, db, fresh.ID, "a@backfill.test", "owner")

	withOps, _ := db.FindOrCreateOrg("backfill-ops")
	opsOwner := addTeamUser(t, db, withOps.ID, "b@backfill.test", "owner")

	custom, _ := db.FindOrCreateOrg("backfill-custom")
	customOwner := addTeamUser(t, db, custom.ID, "c@backfill.test", "owner")
	customYAML := "system:\n  email:\n    - oncall@backfill.test\n"
	if err := db.SaveNotifyAliases(custom.ID, customOwner.ID, customYAML); err != nil {
		t.Fatal(err)
	}

	ownerless, _ := db.FindOrCreateOrg("backfill-ownerless")

	// Rewind the first two orgs to how they were before the rule existed.
	if err := db.DB().Exec(`DELETE FROM notify_configs WHERE org_id = ?`, fresh.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB().Exec(`UPDATE notify_configs SET aliases_yaml = ? WHERE org_id = ?`, opsOnlyYAML, withOps.ID).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.EnsureSystemAliases(); err != nil {
		t.Fatalf("EnsureSystemAliases: %v", err)
	}

	if got := systemEmails(t, db, fresh.ID); len(got) != 1 || got[0] != freshOwner.Email {
		t.Errorf("org with no config: system emails = %v, want [%s]", got, freshOwner.Email)
	}

	nc, _ := db.GetNotifyConfig(withOps.ID)
	aliases, err := ParseNotifyAliases(nc.AliasesYAML)
	if err != nil {
		t.Fatalf("org with aliases: yaml no longer parses: %v\n%s", err, nc.AliasesYAML)
	}
	if aliases["ops"] == nil || len(aliases["ops"].Slack) != 1 {
		t.Errorf("org with aliases: its own alias was lost:\n%s", nc.AliasesYAML)
	}
	if got := systemEmails(t, db, withOps.ID); len(got) != 1 || got[0] != opsOwner.Email {
		t.Errorf("org with aliases: system emails = %v, want [%s]", got, opsOwner.Email)
	}

	if nc, _ := db.GetNotifyConfig(custom.ID); nc.AliasesYAML != customYAML {
		t.Errorf("an org that already had a system alias was rewritten:\n%s", nc.AliasesYAML)
	}

	if _, err := db.GetNotifyConfig(ownerless.ID); err == nil {
		t.Error("an org with no owner got a notify config")
	}

	// Running again changes nothing.
	again, _ := db.GetNotifyConfig(withOps.ID)
	if err := db.EnsureSystemAliases(); err != nil {
		t.Fatal(err)
	}
	if now, _ := db.GetNotifyConfig(withOps.ID); now.AliasesYAML != again.AliasesYAML {
		t.Errorf("second run rewrote the yaml:\n%s", now.AliasesYAML)
	}
}
