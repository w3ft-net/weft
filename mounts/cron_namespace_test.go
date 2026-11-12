//go:build !defaultshape

package mounts

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/warp/pkg/shellcore"

	"github.com/graybeardcorp/weft/store"
)

const cronBody = "# schedule: every 5m\ncat /log/%s/prod/api/app.log | wc -l\n"

func names(ents []fs.DirEntry) []string {
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// A cron lives at /cron.d/<ns>/<name> and is read only by those who can read
// <ns>. Org-level crons are files beside the namespace directories.
func TestCronMount_NamespaceOwnership(t *testing.T) {
	sys := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	sys.Tier = store.TierSwatch
	tx := sys.Store

	user, err := tx.CreateVerifiedUser("cron-ns@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AddTeamMemberWithEmail(sys.OrgID, user.ID, user.Email, orgs.RoleMember); err != nil {
		t.Fatal(err)
	}
	alpha, _ := tx.GetNamespace(sys.OrgID, "alpha")
	if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: user.ID, NamespaceID: alpha.ID, CanRead: true, CanWrite: true}); err != nil {
		t.Fatal(err)
	}
	owner, err := tx.CreateVerifiedUser("cron-ns-owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AddTeamMemberWithEmail(sys.OrgID, owner.ID, owner.Email, store.RoleOwner); err != nil {
		t.Fatal(err)
	}

	member := &CronMount{cfg: asHosted(sys, user.ID)}
	boss := &CronMount{cfg: asHosted(sys, owner.ID)}

	// Writing: the member manages crons in alpha, not beta, and publishes nothing org-wide.
	if err := member.Authorize("alpha/errors", shellcore.PermWrite); err != nil {
		t.Errorf("member writing alpha/errors: %v", err)
	}
	if err := member.Authorize("beta/errors", shellcore.PermWrite); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("member writing beta/errors: err = %v, want ErrNotExist", err)
	}
	if err := member.Authorize("orgwide", shellcore.PermWrite); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("member writing an org-level cron: err = %v, want a permission error", err)
	}
	// Existing org-level crons stay editable by an owner; no new one can be made.
	if err := tx.SaveCronWatch(sys.OrgID, 0, "", "orgwide", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := boss.Authorize("orgwide", shellcore.PermWrite); err != nil {
		t.Errorf("owner editing an existing org-level cron: %v", err)
	}
	if err := boss.Authorize("brandnew", shellcore.PermWrite); !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "/cron.d/<namespace>/brandnew") {
		t.Errorf("owner creating an org-level cron: err = %v, want a pointer to a namespace", err)
	}

	for path, ns := range map[string]string{"alpha/errors": "alpha", "beta/errors": "beta"} {
		if err := boss.WriteFile(path, []byte(strings.Replace(cronBody, "%s", ns, 1))); err != nil {
			t.Fatalf("owner saving %s: %v", path, err)
		}
	}

	// Reading: the member sees alpha and the org-level cron, never beta.
	root, err := member.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(root), []string{"alpha", "orgwide"}; !slices.Equal(got, want) {
		t.Errorf("member /cron.d = %v, want %v", got, want)
	}
	if root, _ = boss.ReadDir(""); !slices.Equal(names(root), []string{"alpha", "beta", "orgwide"}) {
		t.Errorf("owner /cron.d = %v", names(root))
	}
	if ents, _ := member.ReadDir("alpha"); !slices.Equal(names(ents), []string{"errors"}) {
		t.Errorf("/cron.d/alpha = %v", names(ents))
	}
	if err := member.Authorize("beta", shellcore.PermRead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("member reading /cron.d/beta: err = %v, want ErrNotExist", err)
	}
	if err := member.Authorize("beta/errors", shellcore.PermRead); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("member reading /cron.d/beta/errors: err = %v, want ErrNotExist", err)
	}
	if _, err := member.ReadFile("alpha/errors"); err != nil {
		t.Errorf("member reading alpha/errors: %v", err)
	}
}

// A namespace's cron reads its own namespace and whatever its cron user has
// been added to, whoever saves it. Reading more is a grant: usermod -aG y-r x-cron.
func TestCronMount_HomeAndGrantsBoundReads(t *testing.T) {
	cfg := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	cfg.Tier = store.TierSwatch
	// An owner, who reads everything, as the strictest case.
	owner, _ := cfg.Store.CreateVerifiedUser("cron-home-owner@example.com")
	cfg.Store.AddTeamMemberWithEmail(cfg.OrgID, owner.ID, owner.Email, store.RoleOwner)
	m := &CronMount{cfg: asHosted(cfg, owner.ID)}

	if err := m.WriteFile("alpha/ok", []byte(strings.Replace(cronBody, "%s", "alpha", 1))); err != nil {
		t.Errorf("a cron reading its own namespace: %v", err)
	}
	err := m.WriteFile("alpha/leak", []byte(strings.Replace(cronBody, "%s", "beta", 1)))
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "usermod -aG <namespace>-r alpha-cron") {
		t.Errorf("alpha cron reading beta: err = %v, want a permission error naming the grant", err)
	}
	if err := cfg.Store.SetCronUserGroup(cfg.OrgID, "alpha-cron", "beta-r", true); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("alpha/leak", []byte(strings.Replace(cronBody, "%s", "beta", 1))); err != nil {
		t.Errorf("after usermod -aG beta-r alpha-cron: %v", err)
	}
}

// mv re-homes a cron, keeping its row, so its history and results come with it.
func TestCronMount_RenameRehomesACron(t *testing.T) {
	cfg := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	cfg.Tier = store.TierSwatch
	tx := cfg.Store
	owner, _ := tx.CreateVerifiedUser("cron-mv-owner@example.com")
	tx.AddTeamMemberWithEmail(cfg.OrgID, owner.ID, owner.Email, store.RoleOwner)
	m := &CronMount{cfg: asHosted(cfg, owner.ID)}
	alpha, _ := tx.GetNamespace(cfg.OrgID, "alpha")

	if err := tx.SaveCronWatch(cfg.OrgID, 0, "", "legacy", "every 5m", "cat /log/alpha/api/main.log | wc -l", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.SaveCronWatch(cfg.OrgID, 0, "", "wide", "every 5m", "cat /log/beta/api/main.log | wc -l", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.SaveCronWatch(cfg.OrgID, alpha.ID, "", "taken", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := tx.GetCronWatch(cfg.OrgID, 0, "legacy")

	if err := m.Rename("legacy", "alpha/legacy"); err != nil {
		t.Fatalf("mv legacy alpha/legacy: %v", err)
	}
	after, err := tx.GetCronWatch(cfg.OrgID, alpha.ID, "legacy")
	if err != nil || after.ID != before.ID {
		t.Errorf("the moved cron is %+v (err %v), want the same row as %d", after, err, before.ID)
	}
	if _, err := tx.GetCronWatch(cfg.OrgID, 0, "legacy"); err == nil {
		t.Error("the org-level name survived the move")
	}
	if err := m.Rename("wide", "alpha/wide"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("moving a cron that reads beta into alpha: err = %v, want a permission error", err)
	}
	if err := tx.SaveCronWatch(cfg.OrgID, 0, "", "dup", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Rename("dup", "alpha/taken"); err == nil {
		t.Error("moving onto an existing cron should be refused")
	}
	if err := m.Rename("alpha/taken", "alpha"); err == nil {
		t.Error("a cron can't be renamed onto a namespace directory")
	}
}
