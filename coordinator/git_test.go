package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// gitRun runs one shell line as the user through the real coordinator
// and returns everything it printed.
func gitRun(t *testing.T, c *Coordinator, org store.OrgID, userID, line string) string {
	t.Helper()
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := c.Store.GetOrgByID(org)
	res, err := c.Execute(context.Background(), line, tr, hosted.Actor{Org: cust.ID, User: userID})
	if err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	d := drainExec(t, res)
	return d.Text + d.Stderr
}

func TestGitCommand_EndToEnd(t *testing.T) {
	db, org := testStoreAndCustomer(t)
	alice, err := db.CreateUser("alice-git@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserName(alice.ID, "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().WithContext(store.WithOrg(t.Context(), org)).Create(&store.OrgPermission{
		OrgID: org, UserID: alice.ID, CanManageFunctions: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"echo v1", "echo v2", "echo v3"} {
		if err := db.SaveShellFunction(org, alice.ID, "scan", src, "bash"); err != nil {
			t.Fatal(err)
		}
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	// Org-wide and per-path log; a relative path after cd.
	if out := gitRun(t, c, org, alice.ID, "git log --oneline --no-decorate"); strings.Count(out, "bin/scan") != 3 {
		t.Errorf("git log:\n%s", out)
	}
	if out := gitRun(t, c, org, alice.ID, "cd /bin; git log --oneline --no-decorate -- scan"); strings.Count(out, "bin/scan") != 3 {
		t.Errorf("git log -- scan after cd /bin:\n%s", out)
	}

	// Piped output is plain text (no colour), and the content of an old version.
	piped := gitRun(t, c, org, alice.ID, "git show HEAD~2:bin/scan | cat")
	if strings.TrimSpace(piped) != "echo v1" || strings.Contains(piped, "\x1b[") {
		t.Errorf("git show rev:path | cat = %q", piped)
	}

	// restore writes through the mounts.
	gitRun(t, c, org, alice.ID, "git restore --source=HEAD~2 /bin/scan")
	if fn, _ := db.GetShellFunction(org, "scan"); fn.Source != "echo v1" {
		t.Errorf("after restore: %q", fn.Source)
	}

	// The unknown-command error points at `git --help`, so that must work.
	for _, line := range []string{"git --help", "git -h", "git help"} {
		if out := gitRun(t, c, org, alice.ID, line); !strings.Contains(out, "weft supports a subset of git") {
			t.Errorf("%s: %q", line, out)
		}
	}

	// Unsupported and unknown commands are refused, with exit status.
	for line, want := range map[string]string{
		"git rebase -i HEAD~2": "is a git command, but weft doesn't support it",
		"git rebsae":           "is not a git command",
	} {
		if out := gitRun(t, c, org, alice.ID, line); !strings.Contains(out, want) {
			t.Errorf("%s: %q, want %q", line, out, want)
		}
	}
}
