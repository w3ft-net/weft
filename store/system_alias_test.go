package store

import (
	"strings"
	"testing"
)

// Every org with an owner has a `system` alias in notify.yaml: it is where
// platform alerts (a cron disabled or benched) go, so it can be
// changed but never removed. These tests pin the two halves of that promise:
// the alias exists from the moment the org has an owner, and no save can take
// it away.

const opsOnlyYAML = "ops:\n  slack:\n    - \"#alerts\"\n"

func systemEmails(t *testing.T, db *Store, orgID OrgID) []string {
	t.Helper()
	nc, err := db.GetNotifyConfig(orgID)
	if err != nil {
		t.Fatalf("GetNotifyConfig: %v", err)
	}
	aliases, err := ParseNotifyAliases(nc.AliasesYAML)
	if err != nil {
		t.Fatalf("ParseNotifyAliases(%q): %v", nc.AliasesYAML, err)
	}
	a := aliases[SystemAlias]
	if a == nil {
		return nil
	}
	var out []string
	for _, e := range a.Email {
		out = append(out, e.Address)
	}
	return out
}

func TestOwnerStartsWithASystemAlias(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateOrg("sysalias-owner")
	if err != nil {
		t.Fatal(err)
	}
	owner := addTeamUser(t, db, cust.ID, "founder@sysalias.test", "owner")

	got := systemEmails(t, db, cust.ID)
	if len(got) != 1 || got[0] != owner.Email {
		t.Errorf("system alias emails = %v, want [%s]", got, owner.Email)
	}
}

func TestAddingAMemberDoesNotTouchTheSystemAlias(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("sysalias-member")
	owner := addTeamUser(t, db, cust.ID, "founder@sysalias.test", "owner")
	addTeamUser(t, db, cust.ID, "teammate@sysalias.test", "member")

	got := systemEmails(t, db, cust.ID)
	if len(got) != 1 || got[0] != owner.Email {
		t.Errorf("system alias emails = %v, want only the owner [%s]", got, owner.Email)
	}
}

func TestSaveNotifyAliases_SystemAliasCanBeChangedButNotRemoved(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("sysalias-save")
	owner := addTeamUser(t, db, cust.ID, "founder@sysalias.test", "owner")
	before, err := db.GetNotifyConfig(cust.ID)
	if err != nil {
		t.Fatalf("the owner's org has no notify config: %v", err)
	}

	for name, yaml := range map[string]string{
		"dropped":        opsOnlyYAML,
		"emptied":        "",
		"no targets":     "system: {}\n" + opsOnlyYAML,
		"blank document": "# nothing here\n",
	} {
		err := db.SaveNotifyAliases(cust.ID, owner.ID, yaml)
		if err == nil || !strings.Contains(err.Error(), SystemAlias) {
			t.Errorf("%s: err = %v, want a refusal that names the %q alias", name, err, SystemAlias)
		}
	}
	after, err := db.GetNotifyConfig(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AliasesYAML != before.AliasesYAML {
		t.Errorf("a refused save changed the stored yaml:\n%s", after.AliasesYAML)
	}

	changed := "system:\n  email:\n    - pager@sysalias.test\n" + opsOnlyYAML
	if err := db.SaveNotifyAliases(cust.ID, owner.ID, changed); err != nil {
		t.Fatalf("changing the system alias: %v", err)
	}
	if got := systemEmails(t, db, cust.ID); len(got) != 1 || got[0] != "pager@sysalias.test" {
		t.Errorf("system alias emails = %v, want the edited address", got)
	}
}
