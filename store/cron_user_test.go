package store

import (
	"slices"
	"testing"
)

// Every namespace has a cron user, x-cron, which starts in x's read group and
// reads another namespace only once it is added to that namespace's read group.
func TestCronUser_HomeMembershipAndGrants(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("cron-user")
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	beta, _ := db.FindOrCreateNamespace(org.ID, "beta")
	user := CronUserID(alpha.ID)

	readable := func() []string {
		t.Helper()
		got, err := db.ReadableNamespaces(org.ID, user, false)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	if got := readable(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("a new namespace's cron user reads %v, want [alpha]", got)
	}

	if err := db.SetCronUserGroup(org.ID, "alpha-cron", "beta-r", true); err != nil {
		t.Fatal(err)
	}
	if got := readable(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Errorf("after granting beta-r: %v", got)
	}
	if got := db.mustCronGroups(t, org.ID, "alpha-cron"); !slices.Equal(got, []string{"alpha-r", "beta-r"}) {
		t.Errorf("groups = %v", got)
	}

	// Only a read group can be granted to a cron user, and only of a namespace that exists.
	for _, bad := range []string{"beta-w", "beta", "nope-r"} {
		if err := db.SetCronUserGroup(org.ID, "alpha-cron", bad, true); err == nil {
			t.Errorf("granting %q to a cron user should be refused", bad)
		}
	}
	if err := db.SetCronUserGroup(org.ID, "nope-cron", "beta-r", true); err == nil {
		t.Error("a namespace that doesn't exist has no cron user")
	}

	if err := db.SetCronUserGroup(org.ID, "alpha-cron", "beta-r", false); err != nil {
		t.Fatal(err)
	}
	if got := readable(); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("after revoking beta-r: %v", got)
	}

	// Deleting a namespace takes its cron user, and what that user was granted, with it.
	if err := db.SetCronUserGroup(org.ID, "alpha-cron", "beta-r", true); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNamespace(t.Context(), org.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetNamespacePermission(user, beta.ID); err == nil {
		t.Error("a deleted namespace's cron user kept its grant on beta")
	}
}

func (s *Store) mustCronGroups(t *testing.T, org OrgID, name string) []string {
	t.Helper()
	g, err := s.CronUserGroups(org, name)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
