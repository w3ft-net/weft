package shellfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
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
	perms    orgPerms
	wantDeny bool
}

func cronYAML() string {
	return "# schedule: every 1h\necho ok\n"
}

// orgPerms is a member's role and the four org bits on their org_permissions row.
type orgPerms struct {
	Owner, Functions, Watches, Etc, CreateNS bool
}

// ownerPerms is what promotion to owner materializes: every bit. Fixtures that
// just want "a user who can write everything" use this rather than
// orgPerms{Owner: true}, which is an owner holding no bits and so writes nothing.
func ownerPerms() orgPerms {
	return orgPerms{Owner: true, Functions: true, Watches: true, Etc: true, CreateNS: true}
}

// addMember adds a user to org with exactly the given role and org bits and
// returns their ID.
func addMember(t *testing.T, db *store.Store, org store.OrgID, name string, p orgPerms) string {
	t.Helper()
	u, err := db.CreateVerifiedUser(fmt.Sprintf("%s-%d@perms.test", name, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	role := orgs.RoleMember
	if p.Owner {
		role = store.RoleOwner
	}
	if err := db.AddTeamMemberWithEmail(org, u.ID, u.Email, role); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().Exec(`INSERT INTO org_permissions (org_id, user_id, can_manage_functions, can_manage_watches, can_manage_etc, can_create_namespaces)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, user_id) DO UPDATE SET can_manage_functions = EXCLUDED.can_manage_functions,
		can_manage_watches = EXCLUDED.can_manage_watches, can_manage_etc = EXCLUDED.can_manage_etc,
		can_create_namespaces = EXCLUDED.can_create_namespaces`,
		org, u.ID, p.Functions, p.Watches, p.Etc, p.CreateNS).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestOrgPerms_BinMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-bin-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", orgPerms{}, true},
		{"member-with-flag", orgPerms{Functions: true}, false},
		{"owner-no-flag", orgPerms{Owner: true}, true},
		{"member-other-flag", orgPerms{Watches: true, Etc: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(addMember(t, db, cust.ID, tc.name, tc.perms)))
			err := fs.WriteFile("/bin/probe", []byte("echo probe\n"))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

// A namespace's crons are written by a member of its write group, or an owner;
// an org-level cron is published by an owner alone.
func TestOrgPerms_CronMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-cron-%d", os.Getpid()))
	ns, _ := db.FindOrCreateNamespace(cust.ID, "ops")

	cases := []struct {
		name         string
		owner, write bool
		path         string
		wantDeny     bool
	}{
		{"member-without-write", false, false, "/cron.d/ops/probe", true},
		{"member-with-write", false, true, "/cron.d/ops/probe", false},
		{"owner", true, false, "/cron.d/ops/probe", false},
		{"member-with-write-publishing-org-wide", false, true, "/cron.d/probe", true},
		{"owner-publishing-org-wide", true, false, "/cron.d/probe", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := addMember(t, db, cust.ID, tc.name, orgPerms{Owner: tc.owner})
			if err := db.UpsertNamespacePermission(&store.NamespacePermission{UserID: user, NamespaceID: ns.ID, CanRead: true, CanWrite: tc.write}); err != nil {
				t.Fatal(err)
			}
			fs := New(db, cust.ID, nil, WithTier("swatch"), WithUser(user))
			err := fs.WriteFile(tc.path, []byte(cronYAML()))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

func TestOrgPerms_EtcMount_WriteFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg(fmt.Sprintf("perm-etc-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", orgPerms{}, true},
		{"member-with-flag", orgPerms{Etc: true}, false},
		{"owner-no-flag", orgPerms{Owner: true}, true},
		{"member-other-flag", orgPerms{Functions: true, Watches: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(addMember(t, db, cust.ID, tc.name, tc.perms)))
			err := fs.WriteFile("/etc/notify.yaml", []byte("system:\n  email:\n    - \"owner@perms.test\"\n"))
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
