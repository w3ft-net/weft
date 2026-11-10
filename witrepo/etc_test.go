package witrepo

import (
	"fmt"
	"strings"
	"testing"
)

func dash(title string) string {
	return fmt.Sprintf("title: %s\nsections:\n  - tiles:\n      - kind: counter\n        cron: a/errs\n", title)
}

func (f *fixture) saveDash(t *testing.T, name, body string) {
	t.Helper()
	if err := f.db.SaveDashboard(f.org, f.alice.ID, name, body); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) saveNotify(t *testing.T, body string) {
	t.Helper()
	if err := f.db.SaveNotifyAliases(f.org, f.alice.ID, body); err != nil {
		t.Fatal(err)
	}
}

const notifyV1 = "system:\n  email:\n    - \"owner@loom.test\"\nops:\n  email:\n    - \"ops@loom.test\"\n"
const notifyV2 = "system:\n  email:\n    - \"owner@loom.test\"\nops:\n  email:\n    - \"ops@loom.test\"\n    - \"kai@loom.test\"\n"

func logOldestFirst(t *testing.T, r *Repo, args ...string) []string {
	t.Helper()
	return oldestFirst(subjects(git(t, r, "log", append([]string{"--oneline", "--no-decorate"}, args...)...)))
}

func TestEtc_DashboardHistoryAndRestore(t *testing.T) {
	f := newFixture(t)
	f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
	f.saveDash(t, "prod", dash("v1"))
	f.saveDash(t, "prod", dash("v2"))
	f.saveDash(t, "prod", dash("v3"))
	r := f.repo(f.alice, "/")

	got := logOldestFirst(t, r, "--", "/etc/dashboards/prod.yaml")
	want := []string{"Create etc/dashboards/prod.yaml", "Update etc/dashboards/prod.yaml", "Update etc/dashboards/prod.yaml"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log = %v, want %v", got, want)
	}
	if out := git(t, r, "show", "HEAD~1:etc/dashboards/prod.yaml"); !strings.Contains(out, "title: v2") {
		t.Errorf("show HEAD~1 = %q", out)
	}

	git(t, r, "restore", "--source=HEAD~2", "/etc/dashboards/prod.yaml")
	d, err := f.db.GetDashboard(f.org, "prod")
	if err != nil || !strings.Contains(d.YAMLBody, "title: v1") {
		t.Fatalf("after restore: %v %+v", err, d)
	}
}

func TestEtc_DashboardDeletionAndRecreation(t *testing.T) {
	f := newFixture(t)
	f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
	f.saveDash(t, "prod", dash("v1"))
	if err := f.db.DeleteDashboard(f.org, "prod"); err != nil {
		t.Fatal(err)
	}
	f.saveDash(t, "prod", dash("v2"))

	got := logOldestFirst(t, f.repo(f.alice, "/"), "--", "/etc/dashboards")
	want := []string{"Create etc/dashboards/prod.yaml", "Delete etc/dashboards/prod.yaml", "Create etc/dashboards/prod.yaml"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("log = %v, want %v", got, want)
	}
}

func TestEtc_NotifyHistory_IgnoresTheEmptyRowSlackSetupCreates(t *testing.T) {
	f := newFixture(t)
	// Slack setup creates the notify row before any aliases exist.
	if err := f.db.SaveSlackConfig(f.org, "https://hooks.slack.test/x", "", "team"); err != nil {
		t.Fatal(err)
	}
	r := f.repo(f.alice, "/")
	if out := gitErr(t, r, "log", "--oneline"); !strings.Contains(out, "does not have any commits yet") {
		t.Fatalf("empty notify row should not be a commit: %q", out)
	}
	f.saveNotify(t, notifyV1)
	f.saveNotify(t, notifyV2)

	got := logOldestFirst(t, f.repo(f.alice, "/"), "--", "/etc/notify.yaml")
	want := []string{"Create etc/notify.yaml", "Update etc/notify.yaml"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log = %v, want %v", got, want)
	}

	r = f.repo(f.alice, "/etc")
	git(t, r, "restore", "--source=HEAD~1", "notify.yaml")
	nc, _ := f.db.GetNotifyConfig(f.org)
	if nc.AliasesYAML != notifyV1 {
		t.Errorf("after restore: %q", nc.AliasesYAML)
	}
	if nc.SlackWebhook != "https://hooks.slack.test/x" {
		t.Errorf("restore clobbered the Slack config: %q", nc.SlackWebhook)
	}
}

func TestEtc_OnlyDashboardsAndNotifyAreTracked(t *testing.T) {
	f := newFixture(t)
	r := f.repo(f.alice, "/etc")
	for _, p := range []string{"/etc/dashboards", "/etc/dashboards/prod.yaml", "/etc/notify.yaml", "notify.yaml", "dashboards/prod.yaml", "/etc"} {
		if _, err := r.ResolvePath(p); err != nil {
			t.Errorf("ResolvePath(%q): %v", p, err)
		}
	}
	for _, p := range []string{"/etc/passwd", "/etc/usage", "passwd"} {
		if _, err := r.ResolvePath(p); err == nil || !strings.Contains(err.Error(), "outside the repository") {
			t.Errorf("ResolvePath(%q) = %v", p, err)
		}
	}
}

func TestEtc_WritesNeedConfigmgr(t *testing.T) {
	f := newFixture(t)
	f.saveNotify(t, notifyV1)
	f.saveNotify(t, notifyV2)
	out := gitErr(t, f.repo(f.bob, "/"), "restore", "--source=HEAD~1", "/etc/notify.yaml")
	if !strings.Contains(out, "configmgr") {
		t.Errorf("err = %q", out)
	}
	if nc, _ := f.db.GetNotifyConfig(f.org); nc.AliasesYAML != notifyV2 {
		t.Errorf("a refused restore still wrote: %q", nc.AliasesYAML)
	}
}

func TestEtc_RestoreStillValidates(t *testing.T) {
	f := newFixture(t)
	f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
	f.saveDash(t, "prod", dash("v1"))
	f.saveDash(t, "prod", dash("v2"))
	if err := f.db.DeleteCronWatch(f.org, f.nsA, "errs"); err != nil {
		t.Fatal(err)
	}
	out := gitErr(t, f.repo(f.alice, "/"), "restore", "--source=HEAD~2", "/etc/dashboards/prod.yaml")
	if !strings.Contains(out, "does not exist") {
		t.Errorf("err = %q", out)
	}
}

func TestEtc_AmendAndRetentionBoundary(t *testing.T) {
	f := newFixture(t)
	f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
	for i := 1; i <= 8; i++ {
		f.saveDash(t, "prod", dash(fmt.Sprintf("v%d", i)))
	}
	r := f.repo(f.alice, "/")
	out := git(t, r, "log", "--oneline", "--decorate", "--", "/etc/dashboards/prod.yaml")
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 6 {
		t.Errorf("want live + 5 history = 6 commits, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "grafted") {
		t.Errorf("trimmed history should show a grafted boundary:\n%s", out)
	}
	git(t, r, "commit", "--amend", "-m", "Rework the prod dashboard")
	if got := subjects(git(t, f.repo(f.alice, "/"), "log", "--oneline", "--no-decorate", "-1")); got[0] != "Rework the prod dashboard" {
		t.Errorf("amend = %v", got)
	}
}
