//go:build test

package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// sparesProvisioner is a leader provisioner over a tx-scoped store, an
// EC2Provisioner on the fake EC2 (small in us-east-2a/b, medium in 2c) and
// a pool over both.
func sparesProvisioner(t *testing.T) (*Provisioner, *store.Store, *fakeEC2) {
	t.Helper()
	db := openSMTestStore(t)
	f := newFakeEC2()
	ep := NewEC2ProvisionerWithClient(f, &fakeVolumeManager{},
		map[string]map[string]string{
			"small":  {"us-east-2a": "subnet-a", "us-east-2b": "subnet-b"},
			"medium": {"us-east-2c": "subnet-c"},
		},
		map[string]string{"small": "lt-small", "medium": "lt-medium"},
		"ami-1")
	reg := NewShardRegistry()
	reg.SetStore(ShardStore{db})
	p := NewProvisioner(reg, ep, ProvisionerConfig{MaxConcurrentEvaluate: 1})
	p.SetStore(db)
	p.SetSparePool(NewSparePool(ShardStore{db}, ep, time.Now))
	return p, db, f
}

func pairsOf(t *testing.T, p *Provisioner) map[SparePair]bool {
	t.Helper()
	got, err := p.desiredSparePairs()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The pool covers exactly the (tier, AZ) pairs some bucket stripes into,
// plus any pair where an older live shard still sits.
func TestDesiredSparePairs_FollowBucketsAndLiveShards(t *testing.T) {
	p, db, _ := sparesProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis) // small, Target 2 → stripes 0,1 → 2a, 2b
	got := pairsOf(t, p)
	want := map[SparePair]bool{{"small", "us-east-2a"}: true, {"small", "us-east-2b"}: true}
	if len(got) != len(want) || !got[SparePair{"small", "us-east-2a"}] || !got[SparePair{"small", "us-east-2b"}] {
		t.Fatalf("desired pairs: %v, want %v", got, want)
	}

	// An older shard in a pair no bucket stripes into keeps that pair; an
	// archived one doesn't.
	now := time.Now().UTC()
	for _, sh := range []*Shard{
		{ID: "s-old-medium", State: StateReadOnly, Tier: "medium", AZ: "us-east-2c", BucketID: bucket.ID, WindowStart: now, WindowEnd: now, CreatedAt: now},
		{ID: "s-archived", State: StateArchived, Tier: "medium", AZ: "us-east-2d", BucketID: bucket.ID, WindowStart: now, WindowEnd: now, CreatedAt: now},
	} {
		p.registry.Add(sh)
	}
	got = pairsOf(t, p)
	if !got[SparePair{"medium", "us-east-2c"}] {
		t.Fatal("a live shard's pair must keep a spare")
	}
	if got[SparePair{"medium", "us-east-2d"}] {
		t.Fatal("an archived shard has no instance to upgrade; its pair needs no spare")
	}
}

// Deleting the last bucket of a tier (and its shards going away) retires
// the tier's spares on the next tick.
func TestReconcileSpares_RetiresATiersSparesWithItsLastBucket(t *testing.T) {
	p, db, f := sparesProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := db.DB().Exec("UPDATE buckets SET tier = 'medium' WHERE id = ?", bucket.ID).Error; err != nil {
		t.Fatal(err)
	}
	medium := func() int {
		n := 0
		for _, in := range mustListSpares(t, p) {
			if in.Tier == "medium" {
				n++
			}
		}
		return n
	}
	p.reconcileSpares(context.Background())
	if n := medium(); n != 1 {
		t.Fatalf("medium spares after the first tick: %d, want 1 (medium/2c)", n)
	}
	if err := db.DB().Exec("DELETE FROM buckets WHERE id = ?", bucket.ID).Error; err != nil {
		t.Fatal(err)
	}
	p.reconcileSpares(context.Background())
	if n := medium(); n != 0 {
		t.Fatalf("%d medium spares outlived the tier's last bucket", n)
	}
	_ = f
}

// A spare's heartbeat refreshes its pool row and nothing else: it never
// reaches the instance correlation that promotes a LAUNCHING shard.
func TestProcessHeartbeat_ASpareNeverPromotes(t *testing.T) {
	p, db, _ := sparesProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := (ShardStore{db}).InsertSpare(&Spare{InstanceID: "i-sp", Tier: "small", AZ: "us-east-2a", ImageID: "ami-1", LaunchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	launching := &Shard{ID: "s-launching", State: StateLaunching, InstanceID: "i-sp", Tier: "small", BucketID: bucket.ID, WindowStart: now, WindowEnd: now, CreatedAt: now}
	p.registry.Add(launching)
	cfg := &stubConfigurer{}
	p.configurer = cfg

	p.ProcessHeartbeat(store.ShardHeartbeat{InstanceID: "i-sp", Addr: "10.0.0.5:9000", Spare: true, BinarySHA: "abc"})

	cfg.mu.Lock()
	dials := len(cfg.calls)
	cfg.mu.Unlock()
	if dials != 0 {
		t.Fatal("a spare's heartbeat was taken as a shard's first heartbeat")
	}
	rows, _ := (ShardStore{db}).ListSpares()
	if len(rows) != 1 || rows[0].Addr != "10.0.0.5:9000" || rows[0].LastHeartbeatAt == nil || rows[0].BinarySHA != "abc" {
		t.Fatalf("spare row after heartbeat: %+v", rows)
	}
}

func mustListSpares(t *testing.T, p *Provisioner) []SpareInstance {
	t.Helper()
	got, err := p.launcher.(SpareLauncher).ListSpareInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}
