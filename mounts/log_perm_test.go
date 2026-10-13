package mounts

import (
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"

	"github.com/graybeardcorp/weft/store"
)

// Authorizing a member to read /log/<ns> must never create <ns>.
// The old check used FindOrCreateNamespace, so any non-owner who listed
// or stat'd /log/<anything> left a new namespace behind (and then got
// "permission denied", because a fresh namespace has no permission row).
func TestAuthorize_NeverCreatesNamespaces(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	org, err := tx.FindOrCreateOrg("mounts-perm-test")
	if err != nil {
		t.Fatal(err)
	}
	user, err := tx.CreateVerifiedUser("mounts-perm@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tx.AddTeamMemberWithEmail(org.ID, user.ID, user.Email, orgs.RoleMember)

	readable, err := tx.FindOrCreateNamespace(org.ID, "readable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.FindOrCreateNamespace(org.ID, "private"); err != nil {
		t.Fatal(err)
	}
	if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: user.ID, NamespaceID: readable.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}

	m := &LogMount{cfg: Config{Store: tx, OrgID: org.ID, UserID: user.ID}}

	if err := m.Authorize("readable/prod/api"); err != nil {
		t.Errorf("a member with read permission must read their namespace: %v", err)
	}
	if err := m.Authorize("private"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a member without a permission row: err = %v, want ErrNotExist", err)
	}
	for _, typo := range []string{"typo", "readabel", "no such"} {
		if err := m.Authorize(typo); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q does not exist: err = %v, want ErrNotExist", typo, err)
		}
		if _, err := tx.GetNamespace(org.ID, typo); err == nil {
			t.Errorf("probing /log/%s created a namespace", typo)
		}
	}
}

// /log lists the org's namespaces from the namespace table, filtered by the
// caller's read permission — not whatever happens to have registered a
// service. A readable namespace with no services yet must still show.
func TestLogReadDir_RootListsReadableNamespacesFromDB(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	org, err := tx.FindOrCreateOrg("mounts-root-list-test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := tx.FindOrCreateOrg("mounts-root-list-other")
	if err != nil {
		t.Fatal(err)
	}
	user, err := tx.CreateVerifiedUser("mounts-root-list@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tx.AddTeamMemberWithEmail(org.ID, user.ID, user.Email, orgs.RoleMember)

	for _, name := range []string{"withsvc", "empty", "private"} {
		ns, err := tx.FindOrCreateNamespace(org.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "private" {
			continue
		}
		if err := tx.UpsertNamespacePermission(&store.NamespacePermission{UserID: user.ID, NamespaceID: ns.ID, CanRead: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.FindOrCreateNamespace(other.ID, "elsewhere"); err != nil {
		t.Fatal(err)
	}

	services := []string{"withsvc/prod/api/app", "private/prod/api/app"}
	names := func(cfg Config) []string {
		cfg.Store, cfg.OrgID, cfg.LogServices = tx, org.ID, services
		ents, err := (&LogMount{cfg: cfg}).ReadDir("")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}

	if got, want := names(Config{UserID: user.ID}), []string{"empty", "withsvc"}; !slices.Equal(got, want) {
		t.Errorf("member /log = %v, want %v", got, want)
	}
	if got, want := names(Config{UserID: user.ID, IsOwner: true}), []string{"empty", "private", "withsvc"}; !slices.Equal(got, want) {
		t.Errorf("owner /log = %v, want %v", got, want)
	}
}
