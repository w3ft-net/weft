package witrepo

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// Restoring or reverting a cron is a cron save, so it is held to the same rule
// as saving one: the user must be able to read what the cron reads. Owners read
// everything.

func TestRestore_CronMustReadOnlyWhatTheUserMayRead(t *testing.T) {
	f := newFixture(t)
	f.saveCron(t, f.alice, "errs", "tail /log/alpha/api/main.log | grep ERROR")
	f.saveCron(t, f.alice, "errs", "echo nothing")

	// alice may write /cron.d but holds no read permission on alpha.
	msg := gitErr(t, f.repo(f.alice, "/"), "restore", "--source=HEAD~1", "/cron.d/errs")

	if !strings.Contains(msg, "aren't allowed to read") {
		t.Errorf("err = %q, want a refusal naming the read restriction", msg)
	}
	if w, _ := f.db.GetCronWatch(f.org, "errs"); w.Pipeline != "echo nothing" {
		t.Errorf("a refused restore wrote %q", w.Pipeline)
	}
}

func TestRestore_OwnerRestoresAnyCron(t *testing.T) {
	f := newFixture(t)
	if err := f.db.AddTeamMemberWithEmail(f.org, f.alice.ID, f.alice.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	f.saveCron(t, f.alice, "errs", "tail /log/alpha/api/main.log | grep ERROR")
	f.saveCron(t, f.alice, "errs", "echo nothing")

	git(t, f.repo(f.alice, "/"), "restore", "--source=HEAD~1", "/cron.d/errs")

	if w, _ := f.db.GetCronWatch(f.org, "errs"); !strings.Contains(w.Pipeline, "/log/alpha/") {
		t.Errorf("the owner's restore did not write: %q", w.Pipeline)
	}
}
