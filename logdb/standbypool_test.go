//go:build test

package logdb

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeStandbyEC2 is an in-memory StandbyLauncher: instances it launched (or
// the test planted) and the image new ones come up on.
type fakeStandbyEC2 struct {
	mu         sync.Mutex
	image      string
	next       int
	instances  map[string]*StandbyInstance
	launched   []string
	terminated []string
	released   []string
}

func newFakeStandbyEC2(image string) *fakeStandbyEC2 {
	return &fakeStandbyEC2{image: image, instances: map[string]*StandbyInstance{}}
}

func (f *fakeStandbyEC2) LaunchStandby(_ context.Context, tier, az, claimFor string) (StandbyInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	in := &StandbyInstance{InstanceID: fmt.Sprintf("i-spare-%d", f.next), Tier: tier, AZ: az, ImageID: f.image, LaunchedAt: time.Now(), ClaimedBy: claimFor}
	f.instances[in.InstanceID] = in
	f.launched = append(f.launched, in.InstanceID)
	return *in, nil
}

func (f *fakeStandbyEC2) ListStandbyInstances(context.Context) ([]StandbyInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StandbyInstance
	for _, in := range f.instances {
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, nil
}

func (f *fakeStandbyEC2) ClaimInstance(_ context.Context, id, shardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in, ok := f.instances[id]; ok {
		in.ClaimedBy = shardID
	}
	return nil
}

func (f *fakeStandbyEC2) ReleaseStandby(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, id) // no longer listed as a standby; it lives on as a shard
	f.released = append(f.released, id)
	return nil
}

func (f *fakeStandbyEC2) Terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, id)
	f.terminated = append(f.terminated, id)
	return nil
}

func (f *fakeStandbyEC2) CurrentImageID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.image
}

func (f *fakeStandbyEC2) setImage(img string) { f.mu.Lock(); f.image = img; f.mu.Unlock() }

func (f *fakeStandbyEC2) terminatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.terminated...)
}

// poolFixture is a pool over an in-memory store and a fake EC2.
func poolFixture(t *testing.T, image string) (*StandbyPool, *memStandbys, *fakeStandbyEC2) {
	t.Helper()
	ec2 := newFakeStandbyEC2(image)
	ss := &memStandbys{}
	return NewStandbyPool(ss, ec2, time.Now), ss, ec2
}

func heartbeatAll(t *testing.T, ss *memStandbys) {
	t.Helper()
	rows, err := ss.ListStandbys()
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range rows {
		if err := ss.ObserveStandbyHeartbeat(sp.InstanceID, "10.0.0.1:9000", "sha", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func unclaimed(t *testing.T, ss *memStandbys, pair StandbyPair) []Standby {
	t.Helper()
	rows, err := ss.ListStandbys()
	if err != nil {
		t.Fatal(err)
	}
	var out []Standby
	for _, sp := range rows {
		if !sp.Claimed() && sp.Tier == pair.Tier && sp.AZ == pair.AZ {
			out = append(out, sp)
		}
	}
	return out
}

func noClaims(Standby) StandbyUse { return StandbyInUse }

var pairA = StandbyPair{"small", "us-east-2a"}

func TestStandbyPool_LaunchesOneStandbyPerDesiredPair(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	desired := map[StandbyPair]bool{pairA: true}
	for i := 0; i < 3; i++ { // repeated ticks must not pile up standbys
		if err := pool.Reconcile(context.Background(), desired, noClaims); err != nil {
			t.Fatal(err)
		}
	}
	if got := unclaimed(t, ss, pairA); len(got) != 1 {
		t.Fatalf("spares for %v after 3 ticks: %d, want 1", pairA, len(got))
	}
	if len(ec2.launched) != 1 {
		t.Fatalf("launched %d spares, want 1", len(ec2.launched))
	}
}

// A deploy changes the image. The stale standby keeps covering its pair until
// its replacement heartbeats; only then is it terminated.
func TestStandbyPool_ReplacesAStaleImageStandbyNewFirst(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-old")
	ctx := context.Background()
	desired := map[StandbyPair]bool{pairA: true}
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ss)
	old := unclaimed(t, ss, pairA)[0].InstanceID

	ec2.setImage("ami-new")
	for i := 0; i < 2; i++ { // the new standby is launched but still booting
		if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
			t.Fatal(err)
		}
		if contains(ec2.terminatedIDs(), old) {
			t.Fatal("stale spare terminated before its replacement heartbeat: the pair was left uncovered")
		}
	}
	if got := unclaimed(t, ss, pairA); len(got) != 2 {
		t.Fatalf("want the stale spare plus one booting replacement, got %d", len(got))
	}

	heartbeatAll(t, ss)
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	if !contains(ec2.terminatedIDs(), old) {
		t.Fatal("stale spare outlived its heartbeating replacement")
	}
	got := unclaimed(t, ss, pairA)
	if len(got) != 1 || got[0].ImageID != "ami-new" {
		t.Fatalf("after replacement: %+v, want one ami-new spare", got)
	}
}

func TestStandbyPool_RetiresStandbysForPairsNoLongerDesired(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ctx := context.Background()
	if err := pool.Reconcile(ctx, map[StandbyPair]bool{pairA: true}, noClaims); err != nil {
		t.Fatal(err)
	}
	if err := pool.Reconcile(ctx, map[StandbyPair]bool{}, noClaims); err != nil {
		t.Fatal(err)
	}
	if got := unclaimed(t, ss, pairA); len(got) != 0 {
		t.Fatalf("%d spares remain for an undesired pair", len(got))
	}
	if len(ec2.terminatedIDs()) != 1 {
		t.Fatalf("terminated %v, want the one spare", ec2.terminatedIDs())
	}
}

// A standby that never heartbeats is given up on and replaced.
func TestStandbyPool_RetiresAStandbyThatNeverBoots(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ctx := context.Background()
	desired := map[StandbyPair]bool{pairA: true}
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	stuck := unclaimed(t, ss, pairA)[0].InstanceID
	pool.clock = func() time.Time { return time.Now().Add(standbyBootBudget + time.Minute) }
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	if !contains(ec2.terminatedIDs(), stuck) {
		t.Fatal("a spare that never heartbeat was kept")
	}
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	if got := unclaimed(t, ss, pairA); len(got) != 1 || got[0].InstanceID == stuck {
		t.Fatalf("pair not re-covered after retiring the stuck spare: %+v", got)
	}
}

// Only a standby on the current image is claimable; with none, the on-demand
// fallback launches one for the claimant.
func TestStandbyPool_NeverClaimsAnOldImageStandby(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-old")
	ctx := context.Background()
	if err := pool.Reconcile(ctx, map[StandbyPair]bool{pairA: true}, noClaims); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ss)
	ec2.setImage("ami-new")

	sp, err := pool.Claim(ctx, "shard-x", pairA.Tier, pairA.AZ)
	if err != nil {
		t.Fatal(err)
	}
	if sp != nil {
		t.Fatalf("claimed %s on %s while the current image is ami-new", sp.InstanceID, sp.ImageID)
	}
	sp, err = pool.ClaimOrLaunch(ctx, "shard-x", pairA.Tier, pairA.AZ)
	if err != nil || sp == nil {
		t.Fatalf("on-demand fallback: %v %v", sp, err)
	}
	if sp.ImageID != "ami-new" || sp.ClaimedBy == nil || *sp.ClaimedBy != "shard-x" {
		t.Fatalf("on-demand spare: %+v", sp)
	}
	// Idempotent: the claimant asking again gets the same standby.
	again, err := pool.ClaimOrLaunch(ctx, "shard-x", pairA.Tier, pairA.AZ)
	if err != nil || again == nil || again.InstanceID != sp.InstanceID {
		t.Fatalf("second claim: %+v %v, want %s again", again, err, sp.InstanceID)
	}
}

// After a leader change the pool is the table plus EC2: an instance tagged
// as a standby with no row (launched by a leader that died before recording
// it) is adopted, with its claim if it carried one.
func TestStandbyPool_RederivesFromEC2(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ec2.instances["i-orphan"] = &StandbyInstance{InstanceID: "i-orphan", Tier: pairA.Tier, AZ: pairA.AZ, ImageID: "ami-1", LaunchedAt: time.Now()}
	ec2.instances["i-claimed"] = &StandbyInstance{InstanceID: "i-claimed", Tier: pairA.Tier, AZ: pairA.AZ, ImageID: "ami-1", LaunchedAt: time.Now(), ClaimedBy: "shard-y"}
	if err := pool.Reconcile(context.Background(), map[StandbyPair]bool{pairA: true}, noClaims); err != nil {
		t.Fatal(err)
	}
	if len(ec2.launched) != 0 {
		t.Fatalf("launched %v although an adopted spare covers the pair", ec2.launched)
	}
	held, err := pool.Held("shard-y")
	if err != nil || held == nil || held.InstanceID != "i-claimed" {
		t.Fatalf("shard-y's claim not re-derived: %+v %v", held, err)
	}
	if got := unclaimed(t, ss, pairA); len(got) != 1 || got[0].InstanceID != "i-orphan" {
		t.Fatalf("orphan not adopted: %+v", got)
	}
}

// Claimed standbys are settled by their path: adopted ones are forgotten
// (they are shards now), abandoned ones terminated.
func TestStandbyPool_SettlesClaimedStandbys(t *testing.T) {
	pool, _, ec2 := poolFixture(t, "ami-1")
	ctx := context.Background()
	adopted, err := pool.ClaimOrLaunch(ctx, "shard-a", pairA.Tier, pairA.AZ)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := pool.ClaimOrLaunch(ctx, "shard-b", pairA.Tier, pairA.AZ)
	if err != nil {
		t.Fatal(err)
	}
	use := func(sp Standby) StandbyUse {
		if *sp.ClaimedBy == "shard-a" {
			return StandbyAdopted
		}
		return StandbyAbandoned
	}
	if err := pool.Reconcile(ctx, map[StandbyPair]bool{}, use); err != nil {
		t.Fatal(err)
	}
	if contains(ec2.terminatedIDs(), adopted.InstanceID) || !contains(ec2.released, adopted.InstanceID) {
		t.Fatal("an adopted spare must be released, not terminated")
	}
	if !contains(ec2.terminatedIDs(), abandoned.InstanceID) {
		t.Fatal("an abandoned spare must be terminated")
	}
	for _, id := range []string{"shard-a", "shard-b"} {
		if held, _ := pool.Held(id); held != nil {
			t.Fatalf("%s still holds %s", id, held.InstanceID)
		}
	}
}
