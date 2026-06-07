package shellfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/mounts"
)

// orgPermCases drives the three identically-shaped gating tests for
// /bin, /cron.d, and /etc: non-owner without the flag is denied; the
// flag (or owner) lets the write through.
type orgPermCase struct {
	name     string
	perms    mounts.OrgPerms
	wantDeny bool
}

func cronYAML() string {
	return "# schedule: every 1h\necho ok\n"
}

func TestOrgPerms_BinMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("perm-bin-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageFunctions: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, false},
		{"member-other-flag", mounts.OrgPerms{CanManageWatches: true, CanManageEtc: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(fmt.Sprintf("u-%s", tc.name), tc.perms))
			err := fs.WriteFile("/bin/probe", []byte("echo probe\n"))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

func TestOrgPerms_CronMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("perm-cron-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageWatches: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, false},
		{"member-other-flag", mounts.OrgPerms{CanManageFunctions: true, CanManageEtc: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(fmt.Sprintf("u-%s", tc.name), tc.perms))
			err := fs.WriteFile("/cron.d/probe", []byte(cronYAML()))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

func TestOrgPerms_EtcMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("perm-etc-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageEtc: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, false},
		{"member-other-flag", mounts.OrgPerms{CanManageFunctions: true, CanManageWatches: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(fmt.Sprintf("u-%s", tc.name), tc.perms))
			err := fs.WriteFile("/etc/notify.yaml", []byte(""))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

// Note: team-membership permissions are no longer a virtual /etc file.
// /etc/team.yaml was removed; team management is the adduser/deluser
// commands, gated owner-only at the api layer. That gating is tested in
// api/team_commands_test.go (TestTeamCommands_OwnerOnlyGating and
// TestMaybeHandleTeamCommand_Routing), not here.

func assertGate(t *testing.T, err error, wantDeny bool) {
	t.Helper()
	if wantDeny {
		if err == nil {
			t.Fatalf("expected permission denied, got nil")
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("expected fs.ErrPermission, got: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
