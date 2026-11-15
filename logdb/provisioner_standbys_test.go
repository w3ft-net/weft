//go:build test

package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// standbysProvisioner is a leader provisioner over a tx-scoped store, an
// EC2Provisioner on the fake EC2 (small in us-east-2a/b, medium in 2c) and
// a pool over both.
func standbysProvisioner(t *testing.T) (*Provisioner, standbyDB, *fakeEC2) {
	t.Helper()
	db := openStandbyTestStore(t)
	f := newFakeEC2()
	ep := NewEC2ProvisionerWithClient(f, &fakeVolumeManager{},
		map[string]map[string]string{
			"small":  {"us-east-2a": "subnet-a", "us-east-2b": "subnet-b"},
			"medium": {"us-east-2c": "subnet-c"},
		},
		map[string]string{"small": "lt-small", "medium": "lt-medium"},
		"ami-1")
	reg := NewShardRegistry()
	reg.SetStore(db.Store)
	p := NewProvisioner(reg, ep, ProvisionerConfig{MaxConcurrentEvaluate: 1})
	setTestStore(p, db.Store)
	p.SetStandbys(NewStandbyService(NewStandbyPool(db, ep, time.Now), nil, nil))
	return p, db, f
}

func pairsOf(t *testing.T, p *Provisioner) map[StandbyPair]bool {
	t.Helper()
	got, err := p.desiredStandbyPairs()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The pool covers exactly the (tier, AZ) pairs some bucket stripes into,
// plus any pair where an older live shard still sits.
func TestDesiredStandbyPairs_FollowBucketsAndLiveShards(t *testing.T) {
	p, db, _ := standbysProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis) // small, Target 2 → stripes 0,1 → 2a, 2b
	got := pairsOf(t, p)
	want := map[StandbyPair]bool{{"small", "us-east-2a"}: true, {"small", "us-east-2b"}: true}
	if len(got) != len(want) || !got[StandbyPair{"small", "us-east-2a"}] || !got[StandbyPair{"small", "us-east-2b"}] {
		t.Fatalf("desired pairs: %v, want %v", got, want)
	}

	// An older shard in a pair no bucket stripes into keeps that pair; an
	// archived one doesn't.
	now := time.Now().UTC()
	for _, sh := range []*Shard{
		{
			ShardRow: ShardRow{
				ID:          "s-old-medium",
				State:       StateReadOnly,
				Tier:        "medium",
				AZ:          "us-east-2c",
				BucketID:    bucket.ID,
				WindowStart: now,
				WindowEnd:   now,
				CreatedAt:   now,
			},
		},
		{
			ShardRow: ShardRow{
				ID:          "s-archived",
				State:       StateArchived,
				Tier:        "medium",
				AZ:          "us-east-2d",
				BucketID:    bucket.ID,
				WindowStart: now,
				WindowEnd:   now,
				CreatedAt:   now,
			},
		},
	} {
		p.registry.Add(sh)
	}
	got = pairsOf(t, p)
	if !got[StandbyPair{"medium", "us-east-2c"}] {
		t.Fatal("a live shard's pair must keep a spare")
	}
	if got[StandbyPair{"medium", "us-east-2d"}] {
		t.Fatal("an archived shard has no instance to upgrade; its pair needs no spare")
	}
}

// Deleting the last bucket of a tier (and its shards going away) retires
// the tier's standbys on the next tick.
func TestReconcileStandbys_RetiresATiersStandbysWithItsLastBucket(t *testing.T) {
	p, db, f := standbysProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis)
	bucket.Tier = "medium"
	if err := db.SaveBucket(bucket); err != nil {
		t.Fatal(err)
	}
	medium := func() int {
		n := 0
		for _, in := range mustListStandbys(t, p) {
			if in.Tier == "medium" {
				n++
			}
		}
		return n
	}
	p.reconcileStandbys(context.Background())
	if n := medium(); n != 1 {
		t.Fatalf("medium spares after the first tick: %d, want 1 (medium/2c)", n)
	}
	p.SetShardStore(withoutBuckets{Store: db.Store, gone: map[int64]bool{bucket.ID: true}})
	p.reconcileStandbys(context.Background())
	if n := medium(); n != 0 {
		t.Fatalf("%d medium spares outlived the tier's last bucket", n)
	}
	_ = f
}

// A standby's heartbeat refreshes its pool row and nothing else: it never
// reaches the instance correlation that promotes a LAUNCHING shard.
func TestProcessHeartbeat_AStandbyNeverPromotes(t *testing.T) {
	p, db, _ := standbysProvisioner(t)
	bucket := seedBucket(t, db, BucketStasis)
	if err := db.InsertStandby(&Standby{InstanceID: "i-sp", Tier: "small", AZ: "us-east-2a", ImageID: "ami-1", LaunchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	launching := &Shard{
		ShardRow: ShardRow{
			ID:          "s-launching",
			State:       StateLaunching,
			InstanceID:  "i-sp",
			Tier:        "small",
			BucketID:    bucket.ID,
			WindowStart: now,
			WindowEnd:   now,
			CreatedAt:   now,
		},
	}
	p.registry.Add(launching)
	cfg := &stubConfigurer{}
	p.configurer = cfg

	p.ProcessHeartbeat(shardstore.ShardHeartbeat{InstanceID: "i-sp", Addr: "10.0.0.5:9000", Standby: true, BinarySHA: "abc"})

	cfg.mu.Lock()
	dials := len(cfg.calls)
	cfg.mu.Unlock()
	if dials != 0 {
		t.Fatal("a spare's heartbeat was taken as a shard's first heartbeat")
	}
	rows, _ := db.ListStandbys()
	if len(rows) != 1 || rows[0].Addr != "10.0.0.5:9000" || rows[0].LastHeartbeatAt == nil || rows[0].BinarySHA != "abc" {
		t.Fatalf("spare row after heartbeat: %+v", rows)
	}
}

func mustListStandbys(t *testing.T, p *Provisioner) []StandbyInstance {
	t.Helper()
	got, err := p.launcher.(StandbyLauncher).ListStandbyInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}
