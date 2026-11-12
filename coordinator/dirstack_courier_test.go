//go:build !defaultshape

package coordinator

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// The dirstack courier lets a delegated client ship its cached cwd/pushd
// stack with each pipeline and read back the dirstack the pipeline left
// behind (ExecuteResult.FinalDirStack), so the server owns all cd/pushd/
// popd semantics and the client just caches the result. These tests pin
// the coordinator half: seed-in via ExecuteWithDir, read-back via
// FinalDirStack. drainExec exhausts the stream first, so FinalDirStack is
// safely populated by the time we read it (same happens-before edge as
// Notices).
//
// customer is "" here, so custID resolves to 0 and runLocal skips the
// shellfs/query-rewrite block — these are pure-bash dirstack exercises,
// exactly the courier surface. FS-backed interaction is covered by the
// gRPC/HTTP layers (built with the client in Stage 5).

// TestCourier_SeedReadback: a seeded cwd is the interpreter's cwd for the
// pipeline — `pwd` prints it and FinalDirStack echoes it back unchanged
// when the pipeline doesn't move.
func TestCourier_SeedReadback(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	res, err := c.ExecuteWithDir(context.Background(), "pwd", TimeRange{}, hosted.Actor{Org: custID, User: ""}, []string{"/log/acme"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	if got := strings.TrimSpace(d.Text); got != "/log/acme" {
		t.Errorf("pwd = %q, want /log/acme (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/log/acme"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// TestCourier_CdMovesCwd: a `cd` in the shipped line moves the cwd, and
// the new cwd comes back on FinalDirStack — the server owns cd semantics.
func TestCourier_CdMovesCwd(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	res, err := c.ExecuteWithDir(context.Background(), "cd /bin && pwd", TimeRange{}, hosted.Actor{Org: custID, User: ""}, []string{"/tmp"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	if got := strings.TrimSpace(d.Text); got != "/bin" {
		t.Errorf("cd /bin && pwd = %q, want /bin (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/bin"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// TestCourier_RelativeCdResolvesAgainstSeed: a relative `cd` resolves
// against the seeded cwd — the whole point of ferrying the dirstack
// (without it the server would resolve against "/").
func TestCourier_RelativeCdResolvesAgainstSeed(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	res, err := c.ExecuteWithDir(context.Background(), "cd sub && pwd", TimeRange{}, hosted.Actor{Org: custID, User: ""}, []string{"/log/acme"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	d := drainExec(t, res)
	if got := strings.TrimSpace(d.Text); got != "/log/acme/sub" {
		t.Errorf("cd sub && pwd = %q, want /log/acme/sub (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/log/acme/sub"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack = %v, want %v", res.FinalDirStack, want)
	}
}

// TestCourier_PushdReturnsFullStack: pushd grows the stack and the whole
// stack (index 0 == cwd) comes back, so the client caches more than cwd.
func TestCourier_PushdReturnsFullStack(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	res, err := c.ExecuteWithDir(context.Background(), "pushd /bin", TimeRange{}, hosted.Actor{Org: custID, User: ""}, []string{"/tmp"})
	if err != nil {
		t.Fatalf("ExecuteWithDir: %v", err)
	}
	drainExec(t, res)
	if want := []string{"/bin", "/tmp"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack after pushd = %v, want %v", res.FinalDirStack, want)
	}
}

// TestCourier_NilSeedDefaultsToRoot: the non-courier path (plain Execute,
// nil dirstack) is unchanged — cwd "/" and a single-entry stack back.
// Guards backwards-compat for every existing Execute caller.
func TestCourier_NilSeedDefaultsToRoot(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	res, err := c.Execute(context.Background(), "pwd", TimeRange{}, hosted.Actor{Org: custID, User: ""})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	d := drainExec(t, res)
	if got := strings.TrimSpace(d.Text); got != "/" {
		t.Errorf("pwd (no seed) = %q, want / (stderr=%q)", got, d.Stderr)
	}
	if want := []string{"/"}; !reflect.DeepEqual(res.FinalDirStack, want) {
		t.Errorf("FinalDirStack (no seed) = %v, want %v", res.FinalDirStack, want)
	}
}
