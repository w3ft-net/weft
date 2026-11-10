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
	if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: user.ID, NamespaceID: alpha.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.DB().Exec(`INSERT INTO org_permissions (org_id, user_id, can_manage_watches) VALUES (?, ?, true)`, sys.OrgID, user.ID).Error; err != nil {
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
	if err := boss.Authorize("orgwide", shellcore.PermWrite); err != nil {
		t.Errorf("owner writing an org-level cron: %v", err)
	}

	for path, ns := range map[string]string{"alpha/errors": "alpha", "beta/errors": "beta"} {
		if err := boss.WriteFile(path, []byte(strings.Replace(cronBody, "%s", ns, 1))); err != nil {
			t.Fatalf("owner saving %s: %v", path, err)
		}
	}
	if err := boss.WriteFile("orgwide", []byte(strings.Replace(cronBody, "%s", "alpha", 1))); err != nil {
		t.Fatal(err)
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

// A namespace's cron runs with that namespace's authority, so it can't be
// saved reading another namespace, whoever saves it. Reading across namespaces
// is what an org-level cron, or a grant, is for.
func TestCronMount_HomeBoundsReads(t *testing.T) {
	cfg := ownerCfg(t, []string{"alpha/prod/api/app", "beta/prod/api/app"})
	cfg.Tier = store.TierSwatch
	// An owner, who reads everything, as the strictest case.
	owner, _ := cfg.Store.CreateVerifiedUser("cron-home-owner@example.com")
	cfg.Store.AddTeamMemberWithEmail(cfg.OrgID, owner.ID, owner.Email, store.RoleOwner)
	m := &CronMount{cfg: asHosted(cfg, owner.ID)}

	if err := m.WriteFile("alpha/ok", []byte(strings.Replace(cronBody, "%s", "alpha", 1))); err != nil {
		t.Errorf("a cron reading its own namespace: %v", err)
	}
	if err := m.WriteFile("alpha/leak", []byte(strings.Replace(cronBody, "%s", "beta", 1))); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("alpha cron reading beta: err = %v, want a permission error", err)
	}
	if err := m.WriteFile("crossing", []byte(strings.Replace(cronBody, "%s", "beta", 1))); err != nil {
		t.Errorf("an org-level cron may read what its saver reads: %v", err)
	}
}
