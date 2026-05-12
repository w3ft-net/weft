package shellfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevlarcon/weft/mounts"
	"github.com/kevlarcon/weft/store"
)

// orgPermCases drives the three identically-shaped gating tests for
// /bin, /cron.d, and /etc: non-owner without the flag is denied; the
// flag (or owner) lets the write through.
type orgPermCase struct {
	name      string
	perms     mounts.OrgPerms
	wantDeny  bool
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

// /etc/team.yaml gates strictly on IsOwner — even a member with
// CanManageEtc must not be able to grant themselves more perms.
// Owners bypass and the empty-doc write succeeds.
func TestOrgPerms_EtcTeam_OwnerOnly(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("perm-team-%d", os.Getpid()))

	cases := []orgPermCase{
		{"member-no-flag", mounts.OrgPerms{}, true},
		{"member-with-can-manage-etc", mounts.OrgPerms{CanManageEtc: true}, true},
		{"owner", mounts.OrgPerms{IsOwner: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := New(db, cust.ID, nil, WithTier("swatch"),
				WithUser(fmt.Sprintf("u-%s", tc.name), tc.perms))
			err := fs.WriteFile("/etc/team.yaml", []byte(""))
			assertGate(t, err, tc.wantDeny)
		})
	}
}

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

// End-to-end through ShellFS: an owner writes /etc/team.yaml and a
// subsequent reader gets the canonical-serialized form back.
func TestOrgPerms_TeamYAML_RoundTripViaShellFS(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateCustomer(fmt.Sprintf("perm-tyr-%d", os.Getpid()))

	memberEmail := fmt.Sprintf("alice-%d@x.test", os.Getpid())
	member, err := db.CreateUser(memberEmail)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now()
	err = db.CreateTeamMember(&store.TeamMember{
		ID:         uuid.New().String(),
		CustomerID: cust.ID,
		Email:      member.Email,
		UserID:     &member.ID,
		Role:       "member",
		Status:     "active",
		InvitedAt:  now,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	_, _ = db.FindOrCreateNamespace(cust.ID, "api")

	ownerFS := New(db, cust.ID, nil, WithTier("swatch"),
		WithUser("test-owner", mounts.OrgPerms{IsOwner: true}))

	body := fmt.Sprintf("%s:\n  bin: edit\n  cron: read-only\n  etc: read-only\n  namespaces:\n    api:\n      read: true\n", memberEmail)
	if err := ownerFS.WriteFile("/etc/team.yaml", []byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ownerFS.ReadFile("/etc/team.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, memberEmail) {
		t.Fatalf("rebuilt yaml missing member: %s", gotStr)
	}
	if !strings.Contains(gotStr, "bin: edit") {
		t.Errorf("expected bin: edit in:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "api:") {
		t.Errorf("expected api namespace in:\n%s", gotStr)
	}
}
