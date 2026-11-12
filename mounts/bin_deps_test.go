//go:build !defaultshape

package mounts

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/warp/pkg/shellcore"

	"github.com/graybeardcorp/weft/store"
)

// Changing a /bin script needs write on every namespace that has a cron running
// it. A script no cron runs may be changed by any member, and nobody may delete
// one a cron still runs.
func TestBinMount_WritesFollowTheCronsThatRunTheScript(t *testing.T) {
	sys := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	sys.Tier = store.TierSwatch
	tx := sys.Store
	alpha, _ := tx.GetNamespace(sys.OrgID, "alpha")
	beta, _ := tx.GetNamespace(sys.OrgID, "beta")

	member := func(email string, alphaWrite, betaWrite bool) *BinMount {
		u, err := tx.CreateVerifiedUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.AddTeamMemberWithEmail(sys.OrgID, u.ID, u.Email, orgs.RoleMember); err != nil {
			t.Fatal(err)
		}
		for ns, write := range map[uint]bool{alpha.ID: alphaWrite, beta.ID: betaWrite} {
			if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: u.ID, NamespaceID: ns, CanRead: true, CanWrite: write}); err != nil {
				t.Fatal(err)
			}
		}
		return &BinMount{cfg: asHosted(sys, u.ID)}
	}
	alphaWriter := member("bd-alpha@example.com", true, false)
	bothWriter := member("bd-both@example.com", true, true)
	plain := member("bd-plain@example.com", false, false)

	for name, src := range map[string]string{"scan": "cat /log/alpha/api/main.log", "free": "echo hi", "shared": "echo shared"} {
		if err := tx.SaveShellFunction(sys.OrgID, "", name, src, "bash"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		ns   uint
		name string
	}{{alpha.ID, "errors"}, {beta.ID, "other"}} {
		pipeline := "/bin/scan"
		if c.ns == beta.ID {
			pipeline = "/bin/shared"
		}
		if err := tx.SaveCronWatch(sys.OrgID, c.ns, "", c.name, "every 5m", pipeline, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.SaveCronWatch(sys.OrgID, alpha.ID, "", "both", "every 5m", "/bin/shared", true, nil); err != nil {
		t.Fatal(err)
	}

	// A cron may name a script that doesn't exist yet; creating it is then a
	// change to what that cron does.
	if err := tx.SaveCronWatch(sys.OrgID, alpha.ID, "", "waiting", "every 5m", "/bin/later", true, nil); err != nil {
		t.Fatal(err)
	}

	denied := func(m *BinMount, script string) bool {
		err := m.Authorize(script, shellcore.PermWrite)
		return errors.Is(err, fs.ErrPermission)
	}
	for _, c := range []struct {
		who    string
		m      *BinMount
		script string
		deny   bool
	}{
		{"alpha writer edits the script alpha's cron runs", alphaWriter, "scan", false},
		{"a member without alpha-w cannot", plain, "scan", true},
		{"anyone may edit a script no cron runs", plain, "free", false},
		{"a script run from two namespaces needs both", alphaWriter, "shared", true},
		{"...and passes with both", bothWriter, "shared", false},
		{"a new script is anyone's", plain, "brandnew", false},
		{"a script a cron is waiting for is not", plain, "later", true},
		{"...unless they write its namespace", alphaWriter, "later", false},
		{"the system identity writes nothing", &BinMount{cfg: sys}, "free", true},
	} {
		if got := denied(c.m, c.script); got != c.deny {
			t.Errorf("%s: denied = %v, want %v", c.who, got, c.deny)
		}
	}
	if err := plain.Authorize("scan", shellcore.PermWrite); !strings.Contains(err.Error(), "alpha-w") {
		t.Errorf("the refusal should name the group needed: %v", err)
	}

	// A script a cron runs can't be deleted, by anyone, and the refusal names the cron.
	if err := bothWriter.Remove("scan"); err == nil || !strings.Contains(err.Error(), "/cron.d/alpha/errors") {
		t.Errorf("deleting a script a cron runs: err = %v, want the cron named", err)
	}
	if err := plain.Remove("free"); err != nil {
		t.Errorf("deleting a script nothing runs: %v", err)
	}
}

// A cron names the scripts it runs by path, so the scripts it depends on can all
// be found.
func TestCronMount_RequiresBinPaths(t *testing.T) {
	cfg := ownerCfg(t, []string{"alpha/prod/api/app"})
	cfg.Tier = store.TierSwatch
	owner, _ := cfg.Store.CreateVerifiedUser("bd-paths-owner@example.com")
	cfg.Store.AddTeamMemberWithEmail(cfg.OrgID, owner.ID, owner.Email, store.RoleOwner)
	if err := cfg.Store.SaveShellFunction(cfg.OrgID, "", "scan", "cat /log/alpha/api/main.log", "bash"); err != nil {
		t.Fatal(err)
	}
	m := &CronMount{cfg: asHosted(cfg, owner.ID)}

	if err := m.WriteFile("alpha/ok", []byte("# schedule: every 5m\n/bin/scan | wc -l\n")); err != nil {
		t.Errorf("a script by path: %v", err)
	}
	err := m.WriteFile("alpha/bare", []byte("# schedule: every 5m\nscan | wc -l\n"))
	if err == nil || !strings.Contains(err.Error(), "/bin/scan") {
		t.Errorf("a script by its bare name: err = %v, want a pointer to /bin/scan", err)
	}
	err = m.WriteFile("alpha/dynamic", []byte("# schedule: every 5m\ncmd=scan; $cmd | wc -l\n"))
	if err == nil {
		t.Error("a computed command was accepted in a cron")
	}
}
