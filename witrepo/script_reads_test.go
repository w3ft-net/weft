package witrepo

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// Restoring or reverting a cron is a cron save, so it is held to the same rule
// as saving one: a cron reads only its home namespace, and an org-level cron
// only what its owner reads.

func TestRestore_CronMustReadOnlyItsHomeNamespace(t *testing.T) {
	f := newFixture(t)
	f.saveNSCron(t, f.alice, "errs", "tail /log/a/api/main.log | grep ERROR")
	f.saveNSCron(t, f.alice, "errs", "tail /log/alpha/api/main.log | grep ERROR")
	f.saveNSCron(t, f.alice, "errs", "echo nothing")

	// A cron in namespace a can't be restored to a version that reads alpha.
	msg := gitErr(t, f.repo(f.alice, "/"), "restore", "--source=HEAD~1", "/cron.d/a/errs")

	if !strings.Contains(msg, "a-cron can't read") {
		t.Errorf("err = %q, want a refusal naming the home restriction", msg)
	}
	if w, _ := f.db.GetCronWatch(f.org, f.nsA, "errs"); w.Pipeline != "echo nothing" {
		t.Errorf("a refused restore wrote %q", w.Pipeline)
	}
}

// An org-level cron is published by an owner and reads what the owner reads.
func TestRestore_OwnerRestoresAnyCron(t *testing.T) {
	f := newFixture(t)
	if err := f.db.AddTeamMemberWithEmail(f.org, f.alice.ID, f.alice.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}
	f.saveCron(t, f.alice, "errs", "tail /log/alpha/api/main.log | grep ERROR")
	f.saveCron(t, f.alice, "errs", "echo nothing")

	git(t, f.repo(f.alice, "/"), "restore", "--source=HEAD~1", "/cron.d/errs")

	if w, _ := f.db.GetCronWatch(f.org, 0, "errs"); !strings.Contains(w.Pipeline, "/log/alpha/") {
		t.Errorf("the owner's restore did not write: %q", w.Pipeline)
	}
}
