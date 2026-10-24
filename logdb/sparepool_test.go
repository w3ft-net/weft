//go:build test

package logdb

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// fakeSpareEC2 is an in-memory SpareLauncher: instances it launched (or
// the test planted) and the image new ones come up on.
type fakeSpareEC2 struct {
	mu         sync.Mutex
	image      string
	next       int
	instances  map[string]*SpareInstance
	launched   []string
	terminated []string
	released   []string
}

func newFakeSpareEC2(image string) *fakeSpareEC2 {
	return &fakeSpareEC2{image: image, instances: map[string]*SpareInstance{}}
}

func (f *fakeSpareEC2) LaunchSpare(_ context.Context, tier, az, claimFor string) (SpareInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	in := &SpareInstance{InstanceID: fmt.Sprintf("i-spare-%d", f.next), Tier: tier, AZ: az, ImageID: f.image, LaunchedAt: time.Now(), ClaimedBy: claimFor}
	f.instances[in.InstanceID] = in
	f.launched = append(f.launched, in.InstanceID)
	return *in, nil
}

func (f *fakeSpareEC2) ListSpareInstances(context.Context) ([]SpareInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SpareInstance
	for _, in := range f.instances {
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, nil
}

func (f *fakeSpareEC2) ClaimInstance(_ context.Context, id, shardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in, ok := f.instances[id]; ok {
		in.ClaimedBy = shardID
	}
	return nil
}

func (f *fakeSpareEC2) ReleaseSpare(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, id) // no longer listed as a spare; it lives on as a shard
	f.released = append(f.released, id)
	return nil
}

func (f *fakeSpareEC2) Terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, id)
	f.terminated = append(f.terminated, id)
	return nil
}

func (f *fakeSpareEC2) CurrentImageID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.image
}

func (f *fakeSpareEC2) setImage(img string) { f.mu.Lock(); f.image = img; f.mu.Unlock() }

func (f *fakeSpareEC2) terminatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.terminated...)
}

// poolFixture is a pool over a transaction-scoped store and a fake EC2.
func poolFixture(t *testing.T, image string) (*SparePool, ShardStore, *fakeSpareEC2) {
	t.Helper()
	db := openSMTestStore(t)
	ec2 := newFakeSpareEC2(image)
	ss := ShardStore{db}
	return NewSparePool(ss, ec2, time.Now), ss, ec2
}

func heartbeatAll(t *testing.T, ss ShardStore) {
	t.Helper()
	rows, err := ss.ListSpares()
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range rows {
		if err := ss.ObserveSpareHeartbeat(sp.InstanceID, "10.0.0.1:9000", "sha", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func unclaimed(t *testing.T, ss ShardStore, pair SparePair) []Spare {
	t.Helper()
	rows, err := ss.ListSpares()
	if err != nil {
		t.Fatal(err)
	}
	var out []Spare
	for _, sp := range rows {
		if !sp.claimed() && sp.Tier == pair.Tier && sp.AZ == pair.AZ {
			out = append(out, sp)
		}
	}
	return out
}

func noClaims(Spare) SpareUse { return SpareInUse }

var pairA = SparePair{"small", "us-east-2a"}

func TestSparePool_LaunchesOneSparePerDesiredPair(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	desired := map[SparePair]bool{pairA: true}
	for i := 0; i < 3; i++ { // repeated ticks must not pile up spares
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

// A deploy changes the image. The stale spare keeps covering its pair until
// its replacement heartbeats; only then is it terminated.
func TestSparePool_ReplacesAStaleImageSpareNewFirst(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-old")
	ctx := context.Background()
	desired := map[SparePair]bool{pairA: true}
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	heartbeatAll(t, ss)
	old := unclaimed(t, ss, pairA)[0].InstanceID

	ec2.setImage("ami-new")
	for i := 0; i < 2; i++ { // the new spare is launched but still booting
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

func TestSparePool_RetiresSparesForPairsNoLongerDesired(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ctx := context.Background()
	if err := pool.Reconcile(ctx, map[SparePair]bool{pairA: true}, noClaims); err != nil {
		t.Fatal(err)
	}
	if err := pool.Reconcile(ctx, map[SparePair]bool{}, noClaims); err != nil {
		t.Fatal(err)
	}
	if got := unclaimed(t, ss, pairA); len(got) != 0 {
		t.Fatalf("%d spares remain for an undesired pair", len(got))
	}
	if len(ec2.terminatedIDs()) != 1 {
		t.Fatalf("terminated %v, want the one spare", ec2.terminatedIDs())
	}
}

// A spare that never heartbeats is given up on and replaced.
func TestSparePool_RetiresASpareThatNeverBoots(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ctx := context.Background()
	desired := map[SparePair]bool{pairA: true}
	if err := pool.Reconcile(ctx, desired, noClaims); err != nil {
		t.Fatal(err)
	}
	stuck := unclaimed(t, ss, pairA)[0].InstanceID
	pool.clock = func() time.Time { return time.Now().Add(spareBootBudget + time.Minute) }
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

// Only a spare on the current image is claimable; with none, the on-demand
// fallback launches one for the claimant.
func TestSparePool_NeverClaimsAnOldImageSpare(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-old")
	ctx := context.Background()
	if err := pool.Reconcile(ctx, map[SparePair]bool{pairA: true}, noClaims); err != nil {
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
	// Idempotent: the claimant asking again gets the same spare.
	again, err := pool.ClaimOrLaunch(ctx, "shard-x", pairA.Tier, pairA.AZ)
	if err != nil || again == nil || again.InstanceID != sp.InstanceID {
		t.Fatalf("second claim: %+v %v, want %s again", again, err, sp.InstanceID)
	}
}

// After a leader change the pool is the table plus EC2: an instance tagged
// as a spare with no row (launched by a leader that died before recording
// it) is adopted, with its claim if it carried one.
func TestSparePool_RederivesFromEC2(t *testing.T) {
	pool, ss, ec2 := poolFixture(t, "ami-1")
	ec2.instances["i-orphan"] = &SpareInstance{InstanceID: "i-orphan", Tier: pairA.Tier, AZ: pairA.AZ, ImageID: "ami-1", LaunchedAt: time.Now()}
	ec2.instances["i-claimed"] = &SpareInstance{InstanceID: "i-claimed", Tier: pairA.Tier, AZ: pairA.AZ, ImageID: "ami-1", LaunchedAt: time.Now(), ClaimedBy: "shard-y"}
	if err := pool.Reconcile(context.Background(), map[SparePair]bool{pairA: true}, noClaims); err != nil {
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

// Claimed spares are settled by their path: adopted ones are forgotten
// (they are shards now), abandoned ones terminated.
func TestSparePool_SettlesClaimedSpares(t *testing.T) {
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
	use := func(sp Spare) SpareUse {
		if *sp.ClaimedBy == "shard-a" {
			return SpareAdopted
		}
		return SpareAbandoned
	}
	if err := pool.Reconcile(ctx, map[SparePair]bool{}, use); err != nil {
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

// Two paths claiming at once never take the same spare. This needs real
// pooled connections and committed rows.
func TestSparePool_ConcurrentClaimsNeverShareASpare(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ss := ShardStore{db}
	az := fmt.Sprintf("az-conc-%d", time.Now().UnixNano())
	const spares, claimants = 3, 10
	for i := 0; i < spares; i++ {
		id := fmt.Sprintf("i-%s-%d", az, i)
		if err := ss.InsertSpare(&Spare{InstanceID: id, Tier: "small", AZ: az, ImageID: "ami-1", LaunchedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ss.DeleteSpare(id) })
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		won   = map[string]string{} // instance → claimant
		start = make(chan struct{})
	)
	for c := 0; c < claimants; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			<-start
			shardID := fmt.Sprintf("shard-%s-%d", az, c)
			sp, err := ss.ClaimSpare(shardID, "small", az, "ami-1", time.Now())
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if sp == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := won[sp.InstanceID]; dup {
				t.Errorf("%s claimed by both %s and %s", sp.InstanceID, prev, shardID)
			}
			won[sp.InstanceID] = shardID
		}(c)
	}
	close(start)
	wg.Wait()
	if len(won) != spares {
		t.Fatalf("%d spares claimed, want all %d", len(won), spares)
	}
}
