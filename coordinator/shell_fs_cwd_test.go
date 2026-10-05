package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

// These tests pin the server-side execution of FS-navigation builtins
// (ls, tree) for a delegated client whose cwd lives inside /log. The
// client ships the whole line plus its cached dirstack (the courier);
// the coordinator seeds the cwd and runs the line through pkg/bash.
//
// Two failures were observed in prod and are guarded here:
//
//  1. cwd not resolved: builtinLs hands its raw "." argument straight to
//     the FS (pkg/bash builtins don't normalise — that's the embedder's
//     job via bash.NewCwdFS). Without the wrapper the FS cleans "." to
//     "/" and returns the root mount set (bin/ cron.d/ etc/ log/ tmp/),
//     so `ls` inside /log/acme nonsensically printed the root.
//
//  2. empty service list: runLocal built shellfs with a nil LogServices
//     slice, so LogMount had nothing to enumerate — even an absolute
//     `ls /log/acme` (or `tree`) listed nothing. The /api/fs path feeds
//     shellfs from SeenServices; the exec path must do the same.
//
// customer is non-empty here (unlike the courier tests) so custID
// resolves and runLocal takes the shellfs/LogMount branch — that's the
// branch both bugs live in. userID is empty so LogMount.canReadNamespace
// short-circuits true (no per-user permission rows needed).

// seedSeenServices registers two services under namespace "acme": one
// env-less (acme/api → "api.log") and one env-bearing (acme/worker @ prod
// → the "prod/" env directory), so the namespace listing exercises both
// the file and the env-subdirectory arms of LogMount.ReadDir.
func seedAcmeServices(c *Coordinator, customerID store.OrgID) {
	c.SeenServices.Add(int64(customerID), "acme/api", "")
	c.SeenServices.Add(int64(customerID), "acme/worker", "prod")
}

// TestShellLs_ResolvesCwdAndListsNamespace: `ls` with no args inside
// /log/acme lists acme's children, not the root mounts.
func TestShellLs_ResolvesCwdAndListsNamespace(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
	const customer = "weftfuncs-test"
	seedAcmeServices(c, custID)

	res, err := c.ExecuteWithDir(context.Background(), "ls", TimeRange{}, customer, "", []string{"/log/acme"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	got := d.Text

	for _, root := range []string{"bin/", "cron.d/", "etc/", "tmp/"} {
		if strings.Contains(got, root) {
			t.Fatalf("ls in /log/acme leaked root mount %q — cwd not resolved against seeded dir\noutput:\n%s\nstderr: %s", root, got, d.Stderr)
		}
	}
	if !strings.Contains(got, "api.log") {
		t.Errorf("ls did not list service api.log\noutput:\n%s\nstderr: %s", got, d.Stderr)
	}
	if !strings.Contains(got, "prod") {
		t.Errorf("ls did not list env directory prod\noutput:\n%s\nstderr: %s", got, d.Stderr)
	}
}

// TestShellTail_RelativeSourceResolvesAgainstCwd reproduces the prod
// report: from /log/vimgym/api, `tail gamestate.log` was mis-parsed as
// the namespace "gamestate" and rejected with "unknown source:
// gamestate" because the planner never resolved the relative source arg
// against the seeded cwd. With the fix it resolves to
// /log/vimgym/api/gamestate.log (namespace vimgym, which the customer
// owns) and runs cleanly — no shards here, so the result is just empty.
func TestShellTail_RelativeSourceResolvesAgainstCwd(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	const customer = "weftfuncs-test"
	if _, err := db.FindOrCreateNamespace(custID, "vimgym"); err != nil {
		t.Fatalf("FindOrCreateNamespace: %v", err)
	}
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})

	res, err := c.ExecuteWithDir(context.Background(), "tail gamestate.log", TimeRange{}, customer, "", []string{"/log/vimgym/api"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	if strings.Contains(d.Stderr, "unknown source") {
		t.Fatalf("tail gamestate.log from /log/vimgym/api was not resolved against cwd: %s", d.Stderr)
	}
}

// TestShellTree_ListsNamespaceFromSeededCwd: `tree` with no args inside
// /log/acme renders acme's subtree. tree (unlike ls) normalises its
// target against cwd itself, so it never tripped the cwd bug — but it
// shared the nil-LogServices bug and rendered an empty tree. This pins
// that tree works once the service list is populated.
func TestShellTree_ListsNamespaceFromSeededCwd(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
	const customer = "weftfuncs-test"
	seedAcmeServices(c, custID)

	res, err := c.ExecuteWithDir(context.Background(), "tree", TimeRange{}, customer, "", []string{"/log/acme"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	got := d.Text

	if !strings.Contains(got, "api") {
		t.Errorf("tree did not include service api\noutput:\n%s\nstderr: %s", got, d.Stderr)
	}
	if !strings.Contains(got, "worker") {
		t.Errorf("tree did not include service worker\noutput:\n%s\nstderr: %s", got, d.Stderr)
	}
}
