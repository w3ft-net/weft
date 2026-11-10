package hosted

import (
	"context"
	"slices"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })
	return tx
}

func TestReadScope(t *testing.T) {
	db := newStore(t)
	org, _ := db.FindOrCreateOrg("hosted-scoper")
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	db.FindOrCreateNamespace(org.ID, "beta")
	mk := func(email string, role orgs.Role) string {
		u, err := db.CreateVerifiedUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddTeamMemberWithEmail(org.ID, u.ID, u.Email, role); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	owner, reader, none := mk("o@hosted.test", store.RoleOwner), mk("r@hosted.test", orgs.RoleMember), mk("n@hosted.test", orgs.RoleMember)
	if err := db.UpsertNamespacePermission(&store.NamespacePermission{UserID: reader, NamespaceID: alpha.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	sc := Scoper{Store: db}
	scope := func(user string) ([]string, bool) {
		t.Helper()
		set, err := sc.ReadScope(context.Background(), Actor{Org: org.ID, User: user})
		if err != nil {
			t.Fatal(err)
		}
		if err := set.Validate(); err != nil {
			t.Fatal(err)
		}
		names, all, err := set.Namespaces(int64(org.ID))
		if err != nil {
			t.Fatal(err)
		}
		return names, all
	}

	if _, all := scope(owner); !all {
		t.Error("owner: want every namespace")
	}
	if _, all := scope(""); !all {
		t.Error("system identity: want every namespace")
	}
	if names, all := scope(reader); all || !slices.Equal(names, []string{"alpha"}) {
		t.Errorf("reader = %v all=%v, want [alpha]", names, all)
	}
	if names, all := scope(none); all || len(names) != 0 {
		t.Errorf("no grants = %v all=%v, want nothing", names, all)
	}
}

func TestReadScope_FailsClosed(t *testing.T) {
	sc := Scoper{Store: newStore(t)}
	if _, err := sc.ReadScope(context.Background(), Actor{}); err == nil {
		t.Error("actor with no org: want an error, not a scope")
	}
}

// A namespace's cron user reads its home and whatever read groups it has been
// added to; the scoper treats it like any other subject.
func TestReadScope_CronUser(t *testing.T) {
	db := newStore(t)
	org, _ := db.FindOrCreateOrg("hosted-cron-scope")
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	db.FindOrCreateNamespace(org.ID, "beta")
	sc := Scoper{Store: db}
	names := func() []string {
		set, err := sc.ReadScope(context.Background(), Actor{Org: org.ID, User: store.CronUserID(alpha.ID)})
		if err != nil {
			t.Fatal(err)
		}
		n, all, err := set.Namespaces(int64(org.ID))
		if err != nil || all {
			t.Fatalf("names=%v all=%v err=%v", n, all, err)
		}
		return n
	}
	if got := names(); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("cron user reads %v, want [alpha]", got)
	}
	if err := db.SetCronUserGroup(org.ID, "alpha-cron", "beta-r", true); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("after beta-r: %v", got)
	}
}
