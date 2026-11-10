package witrepo

import (
	"strings"
	"testing"
)

// Every wit write goes through the same mounts as the shell, so every one
// needs the group the shell needs: binmgr for /bin, cronmgr for /cron.d,
// configmgr for /etc. bob holds no group; alice holds all of them.

// seeded is a file with two versions written by alice, and how to see which
// one is live.
type seeded struct {
	name, path, group string
	seed              func(*testing.T, *fixture)
	live              func(*fixture) string
	older, newer      string // what live() returns for the first and second version
}

func seededFiles() []seeded {
	return []seeded{
		{
			name: "bin", path: "/bin/scan", group: "binmgr",
			seed: func(t *testing.T, f *fixture) {
				f.saveBin(t, f.alice, "scan", "echo 1")
				f.saveBin(t, f.alice, "scan", "echo 2")
			},
			live:  func(f *fixture) string { fn, _ := f.db.GetShellFunction(f.org, "scan"); return fn.Source },
			older: "echo 1", newer: "echo 2",
		},
		{
			name: "cron", path: "/cron.d/a/errs", group: "a-w",
			seed: func(t *testing.T, f *fixture) {
				f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
				f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep WARN")
			},
			live:  func(f *fixture) string { w, _ := f.db.GetCronWatch(f.org, f.nsA, "errs"); return w.Pipeline },
			older: "tail /log/a/b/c.log | grep ERROR", newer: "tail /log/a/b/c.log | grep WARN",
		},
		{
			name: "dashboard", path: "/etc/dashboards/prod.yaml", group: "configmgr",
			seed: func(t *testing.T, f *fixture) {
				f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
				f.saveDash(t, "prod", dash("v1"))
				f.saveDash(t, "prod", dash("v2"))
			},
			live:  func(f *fixture) string { d, _ := f.db.GetDashboard(f.org, "prod"); return d.YAMLBody },
			older: dash("v1"), newer: dash("v2"),
		},
		{
			name: "notify", path: "/etc/notify.yaml", group: "configmgr",
			seed: func(t *testing.T, f *fixture) {
				f.saveNotify(t, notifyV1)
				f.saveNotify(t, notifyV2)
			},
			live:  func(f *fixture) string { nc, _ := f.db.GetNotifyConfig(f.org); return nc.AliasesYAML },
			older: notifyV1, newer: notifyV2,
		},
	}
}

func TestWrites_RevertNeedsTheMountsGroup(t *testing.T) {
	for _, tc := range seededFiles() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.seed(t, f)

			msg := gitErr(t, f.repo(f.bob, "/"), "revert", "HEAD")

			if !strings.Contains(msg, tc.group) {
				t.Errorf("err = %q, want a %s denial", msg, tc.group)
			}
			if got := tc.live(f); got != tc.newer {
				t.Errorf("a refused revert wrote %q", got)
			}
		})
	}
}

func TestWrites_RestoreNeedsTheMountsGroup(t *testing.T) {
	for _, tc := range seededFiles() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.seed(t, f)

			msg := gitErr(t, f.repo(f.bob, "/"), "restore", "--source=HEAD~1", tc.path)

			if !strings.Contains(msg, tc.group) {
				t.Errorf("err = %q, want a %s denial", msg, tc.group)
			}
			if got := tc.live(f); got != tc.newer {
				t.Errorf("a refused restore wrote %q", got)
			}
		})
	}
}

// Rewording a commit is a write to the file, so it takes the file's group even
// for the author of the version: bob wrote the live version (the store does
// not check groups) but holds none, as if the group had since been revoked.
func TestWrites_RewordNeedsTheMountsGroup(t *testing.T) {
	cases := []struct {
		name, group string
		write       func(*testing.T, *fixture)
	}{
		{"bin", "binmgr", func(t *testing.T, f *fixture) { f.saveBin(t, f.bob, "scan", "echo 1") }},
		{"cron", "a-w", func(t *testing.T, f *fixture) { f.saveNSCron(t, f.bob, "errs", "tail /log/a/b/c.log | grep ERROR") }},
		{"dashboard", "configmgr", func(t *testing.T, f *fixture) {
			f.saveNSCron(t, f.alice, "errs", "tail /log/a/b/c.log | grep ERROR")
			if err := f.db.SaveDashboard(f.org, f.bob.ID, "prod", dash("v1")); err != nil {
				t.Fatal(err)
			}
		}},
		{"notify", "configmgr", func(t *testing.T, f *fixture) {
			if err := f.db.SaveNotifyAliases(f.org, f.bob.ID, notifyV1); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.write(t, f)

			msg := gitErr(t, f.repo(f.bob, "/"), "commit", "--amend", "-m", "sneaky")

			if !strings.Contains(msg, tc.group) {
				t.Errorf("err = %q, want a %s denial", msg, tc.group)
			}
			if out := git(t, f.repo(f.alice, "/"), "log", "-1", "--oneline", "--no-decorate"); strings.Contains(out, "sneaky") {
				t.Errorf("a refused reword still landed: %q", out)
			}
		})
	}
}
