package mounts

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/warp/pkg/shellcore"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// Crons run as the system identity, which reads every namespace, so the limit
// on what a cron reads is applied when it is saved: the saver must be allowed
// to read everything the cron reads, as worked out from its text. A /bin script
// is part of every cron that runs it, so saving one is held to the same rule
// for the crons it would change.

type readsFixture struct {
	db  *store.Store
	org *store.Organization

	owner, reader string // user ids: org owner; member who reads alpha and manages crons and scripts
}

func newReadsFixture(t *testing.T) *readsFixture {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })

	org, err := tx.FindOrCreateOrg("script-reads")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: org.ID, Tier: string(store.TierYard), Status: "active", StartsAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	alpha, _ := tx.FindOrCreateNamespace(org.ID, "alpha")
	tx.FindOrCreateNamespace(org.ID, "beta")

	mk := func(email string, role orgs.Role) string {
		u, err := tx.CreateVerifiedUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.AddTeamMemberWithEmail(org.ID, u.ID, u.Email, role); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	f := &readsFixture{db: tx, org: org,
		owner:  mk("reads-owner@example.test", store.RoleOwner),
		reader: mk("reads-reader@example.test", orgs.RoleMember),
	}
	if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: f.reader, NamespaceID: alpha.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

// fs returns the shell filesystem as user sees it.
func (f *readsFixture) fs(user string) *shellcore.MountFS {
	cfg := asHosted(Config{Store: f.db, OrgID: f.org.ID, Tier: store.TierYard}, user)
	cfg.Authz = grants{tenancy.ActionManageBin: true, tenancy.ActionManageCron: true, tenancy.ActionPublishOrgWide: true, tenancy.ActionManageEtc: true}
	return New(cfg)
}

func cronFile(pipeline string) []byte { return []byte("# schedule: */5 * * * *\n" + pipeline + "\n") }

func (f *readsFixture) seedCron(t *testing.T, name, pipeline string) {
	t.Helper()
	if err := f.db.SaveCronWatch(f.org.ID, 0, f.owner, name, "*/5 * * * *", pipeline, true, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *readsFixture) seedBin(t *testing.T, name, src string) {
	t.Helper()
	if err := f.db.SaveShellFunction(f.org.ID, f.owner, name, src, "bash"); err != nil {
		t.Fatal(err)
	}
}

func TestCronSave_SaverMustBeAbleToReadWhatItReads(t *testing.T) {
	f := newReadsFixture(t)
	// New org-level crons can't be made; this is an edit of an existing one.
	f.seedCron(t, "c", "echo seed")
	cases := []struct {
		name, pipeline string
		user           string
		wantPerm       bool   // refused as a permission problem
		wantRefuse     string // refused as unresolvable, mentioning this
	}{
		{name: "reader, readable namespace", pipeline: "cat /log/alpha/api/main.log | wc -l", user: f.reader},
		{name: "reader, unreadable namespace", pipeline: "cat /log/beta/api/main.log | wc -l", user: f.reader, wantPerm: true},
		{name: "reader, one of each", pipeline: "cat /log/alpha/a.log /log/beta/b.log", user: f.reader, wantPerm: true},
		{name: "reader, every namespace by glob", pipeline: "cat /log/*/api/main.log", user: f.reader, wantPerm: true},
		{name: "reader, listing /log", pipeline: "ls /log", user: f.reader, wantPerm: true},
		{name: "reader, a namespace that does not exist yet", pipeline: "cat /log/future/api/main.log", user: f.reader, wantPerm: true},
		{name: "reader, trace of everything", pipeline: "trace", user: f.reader, wantPerm: true},
		{name: "reader, trace of their namespace", pipeline: "trace alpha", user: f.reader},
		{name: "reader, a computed path", pipeline: `cat "$FILE"`, user: f.reader, wantRefuse: "FILE"},
		{name: "reader, eval", pipeline: `eval "ls"`, user: f.reader, wantRefuse: "eval"},
		{name: "reader, no logs at all", pipeline: "echo hello", user: f.reader},
		{name: "owner, any namespace", pipeline: "cat /log/beta/api/main.log", user: f.owner},
		{name: "owner, a computed path", pipeline: `cat "$FILE"`, user: f.owner},
		{name: "system identity, any namespace", pipeline: "cat /log/beta/api/main.log", user: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.fs(tc.user).WriteFile("/cron.d/c", cronFile(tc.pipeline))
			switch {
			case tc.wantPerm:
				if !errors.Is(err, fs.ErrPermission) {
					t.Errorf("err = %v, want a permission refusal", err)
				}
			case tc.wantRefuse != "":
				if err == nil || errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), tc.wantRefuse) {
					t.Errorf("err = %v, want an unresolvable refusal mentioning %q", err, tc.wantRefuse)
				}
			default:
				if err != nil {
					t.Errorf("err = %v, want the save to succeed", err)
				}
			}
		})
	}
}

// The refusal says the cron reads namespaces the saver can't, not which: a
// glob or a listing would otherwise name the ones they may not see.
func TestCronSave_RefusalDoesNotNameUnreadableNamespaces(t *testing.T) {
	f := newReadsFixture(t)

	err := f.fs(f.reader).WriteFile("/cron.d/c", cronFile("ls /log"))

	if err == nil || strings.Contains(err.Error(), "beta") {
		t.Errorf("err = %v, want a refusal that does not name beta", err)
	}
}

func TestBinSave_CannotWidenWhatAnExistingCronReads(t *testing.T) {
	f := newReadsFixture(t)
	f.seedCron(t, "report", "report | wc -l") // a cron that runs the script `report`

	// There is no `report` yet. Creating one that reads beta would make the
	// existing cron read it.
	err := f.fs(f.reader).WriteFile("/bin/report", []byte("cat /log/beta/api/main.log"))
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a script reading an unreadable namespace, run by a cron: err = %v, want a permission refusal", err)
	}
	if _, err := f.db.GetShellFunction(f.org.ID, "report"); err == nil {
		t.Error("the refused script was saved")
	}

	// One that reads only what the editor can read is fine.
	if err := f.fs(f.reader).WriteFile("/bin/report", []byte("cat /log/alpha/api/main.log")); err != nil {
		t.Errorf("a script reading a readable namespace: %v", err)
	}
	// Narrowing it, or leaving what it reads alone, is fine.
	if err := f.fs(f.reader).WriteFile("/bin/report", []byte("cat /log/alpha/api/main.log | head -n 5")); err != nil {
		t.Errorf("editing without widening: %v", err)
	}
	// Making it unresolvable is not: the cron could no longer be vouched for.
	err = f.fs(f.reader).WriteFile("/bin/report", []byte(`cat "$FILE"`))
	if err == nil || errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "FILE") {
		t.Errorf("a script that became unresolvable under a cron: err = %v, want an unresolvable refusal", err)
	}
}

func TestBinSave_UnreferencedScriptIsUnrestricted(t *testing.T) {
	f := newReadsFixture(t)
	f.seedCron(t, "other", "echo hi")

	// No cron runs it, and anyone who runs it interactively is held to their
	// own read access when it executes.
	if err := f.fs(f.reader).WriteFile("/bin/adhoc", []byte("cat /log/beta/api/main.log")); err != nil {
		t.Errorf("a script no cron runs: %v", err)
	}
}

func TestBinSave_ReachesCronsThroughOtherScripts(t *testing.T) {
	f := newReadsFixture(t)
	f.seedBin(t, "outer", "inner")
	f.seedCron(t, "c", "outer")

	err := f.fs(f.reader).WriteFile("/bin/inner", []byte("cat /log/beta/api/main.log"))

	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a script run by a cron through another script: err = %v, want a permission refusal", err)
	}
}

func TestBinSave_OwnersAndLuaAreUnrestricted(t *testing.T) {
	f := newReadsFixture(t)
	f.seedCron(t, "report", "report | wc -l")

	if err := f.fs(f.owner).WriteFile("/bin/report", []byte("cat /log/beta/api/main.log")); err != nil {
		t.Errorf("owner: %v", err)
	}
	// A Lua script reads only its input, so it can't widen anything.
	if err := f.fs(f.reader).WriteFile("/bin/report.lua", []byte("print('x')")); err != nil {
		t.Errorf("Lua script: %v", err)
	}
}
