package shellfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/graybeardcorp/weft/mounts"
)

// orgPermCase drives the three identically-shaped gating tests for
// /bin, /cron.d, and /etc: the corresponding flag is what lets a write
// through, and nothing else does.
//
// The owner-no-flag case is the one that changed in migration 072. Role
// used to be a second permission authority — IsOwner bypassed the flag
// entirely — so an owner wrote /bin whether or not they held binmgr.
// Now the flag is the only answer. In practice an owner does hold it,
// because becoming an admin materializes all four bits; what this case
// pins is that the bypass is gone, so clearing an owner's row actually
// takes their access away.
type orgPermCase struct {
	name     string
	perms    mounts.OrgPerms
	wantDeny bool
}

func cronYAML() string {
	return "# schedule: every 1h\necho ok\n"
}

// ownerPerms is the permission bundle BuildSession hands an owner: the
// role flag plus the four org bits, which migration 072 materializes on
// promotion. Fixtures that just want "a user who can write everything"
// use this rather than OrgPerms{IsOwner: true} — since the owner bypass
// was removed, that literal denies every write.
func ownerPerms() mounts.OrgPerms {
	return mounts.OrgPerms{
		IsOwner:             true,
		CanManageFunctions:  true,
		CanManageWatches:    true,
		CanManageEtc:        true,
		CanCreateNamespaces: true,
	}
}

func TestOrgPerms_BinMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-bin-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageFunctions: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, true},
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
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-cron-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageWatches: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, true},
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
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-etc-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-flag", mounts.OrgPerms{CanManageEtc: true}, false},
		{"owner-no-flag", mounts.OrgPerms{IsOwner: true}, true},
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
