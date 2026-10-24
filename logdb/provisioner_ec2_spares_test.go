//go:build test

package logdb

import (
	"context"
	"testing"
	"time"
)

func spareProvisioner(f *fakeEC2) *EC2Provisioner {
	p := newTestEC2Provisioner(f, &fakeVolumeManager{})
	p.imageID = "ami-current"
	return p
}

// A pool spare is a shard instance from the tier's template, in the AZ's
// subnet, on the current image, tagged so it boots in spare mode and so
// the shard paths (findLiveByShard, instance correlation) never see it.
func TestEC2Provisioner_LaunchSpareTagsAPoolInstance(t *testing.T) {
	f := newFakeEC2()
	p := spareProvisioner(f)
	in, err := p.LaunchSpare(context.Background(), "small", "us-east-2b", "")
	if err != nil {
		t.Fatalf("LaunchSpare: %v", err)
	}
	if in.ImageID != "ami-current" || in.Tier != "small" || in.AZ != "us-east-2b" || in.ClaimedBy != "" {
		t.Fatalf("spare: %+v", in)
	}
	inst := f.find(in.InstanceID)
	if inst.imageID != "ami-current" {
		t.Fatalf("launched on %q, want the current image", inst.imageID)
	}
	for k, want := range map[string]string{"shard_spare": "true", "shard_tier": "small", "shard_az": "us-east-2b"} {
		if inst.tags[k] != want {
			t.Errorf("tag %s = %q, want %q", k, inst.tags[k], want)
		}
	}
	if _, has := inst.tags["shard_id"]; has {
		t.Error("an unclaimed spare must carry no shard_id tag")
	}
}

// An on-demand spare is born claimed, so a leader that dies before
// recording it leaves an instance the next leader re-derives the claim
// from.
func TestEC2Provisioner_LaunchSpareForAClaimant(t *testing.T) {
	f := newFakeEC2()
	p := spareProvisioner(f)
	in, err := p.LaunchSpare(context.Background(), "small", "us-east-2a", "shard-x")
	if err != nil {
		t.Fatal(err)
	}
	if f.find(in.InstanceID).tags["shard_id"] != "shard-x" || in.ClaimedBy != "shard-x" {
		t.Fatalf("on-demand spare not stamped with its claimant: %+v tags=%v", in, f.find(in.InstanceID).tags)
	}
	got, err := p.ListSpareInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClaimedBy != "shard-x" || got[0].Tier != "small" || got[0].AZ != "us-east-2a" || got[0].ImageID != "ami-current" {
		t.Fatalf("ListSpareInstances: %+v", got)
	}
}

// Claim stamps the claimant; release drops the spare tag, after which the
// instance is the shard's and no longer listed as a spare. Terminated
// spares aren't listed either.
func TestEC2Provisioner_ClaimReleaseAndListSpares(t *testing.T) {
	f := newFakeEC2()
	p := spareProvisioner(f)
	ctx := context.Background()
	a, _ := p.LaunchSpare(ctx, "small", "us-east-2a", "")
	b, _ := p.LaunchSpare(ctx, "small", "us-east-2a", "")
	c, _ := p.LaunchSpare(ctx, "small", "us-east-2b", "")
	if err := p.ClaimInstance(ctx, a.InstanceID, "shard-a"); err != nil {
		t.Fatal(err)
	}
	if f.find(a.InstanceID).tags["shard_id"] != "shard-a" {
		t.Fatal("claim did not stamp shard_id")
	}
	if err := p.ReleaseSpare(ctx, b.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(ctx, c.InstanceID); err != nil {
		t.Fatal(err)
	}
	got, err := p.ListSpareInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InstanceID != a.InstanceID || got[0].ClaimedBy != "shard-a" {
		t.Fatalf("ListSpareInstances: %+v, want only the claimed %s", got, a.InstanceID)
	}
}

// pooledProvisioner is an EC2Provisioner whose launches claim from a pool
// holding one heartbeating current-image spare in us-east-2a.
func pooledProvisioner(t *testing.T, vols VolumeManager) (*EC2Provisioner, *fakeEC2, string) {
	t.Helper()
	f := newFakeEC2()
	p := newTestEC2Provisioner(f, vols)
	p.imageID = "ami-current"
	db := openSMTestStore(t)
	pool := NewSparePool(ShardStore{db}, p, time.Now)
	p.SetSparePool(pool)
	if err := pool.Reconcile(context.Background(), map[SparePair]bool{{"small", "us-east-2a"}: true}, func(Spare) SpareUse { return SpareInUse }); err != nil {
		t.Fatal(err)
	}
	spares, _ := p.ListSpareInstances(context.Background())
	if len(spares) != 1 {
		t.Fatalf("pool seeded %d spares", len(spares))
	}
	if err := (ShardStore{db}).ObserveSpareHeartbeat(spares[0].InstanceID, "10.0.0.9:9000", "sha", time.Now()); err != nil {
		t.Fatal(err)
	}
	return p, f, spares[0].InstanceID
}

// A recovery claims the spare instead of booting a fresh instance, and
// force-detaches the volume from the dead instance rather than waiting
// for EC2 to release it.
func TestEC2Provisioner_RecoveryClaimsASpare(t *testing.T) {
	vols := &swapVolumes{volume: "vol-1", shardID: "s-rec", attachedTo: "i-dead"}
	p, f, spare := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-rec", Tier: "small", AZ: "us-east-2a", ExistingVolumeID: "vol-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != spare {
		t.Fatalf("recovery landed on %s, want the claimed spare %s", res.InstanceID, spare)
	}
	if len(f.insts) != before {
		t.Fatal("a recovery with a spare available ran a fresh instance")
	}
	if len(vols.detaches) != 1 || !vols.detaches[0] {
		t.Fatalf("detaches %v: want one forced detach from the dead instance", vols.detaches)
	}
	if vols.where() != spare {
		t.Fatalf("volume attached to %q, want the spare", vols.where())
	}
	if f.find(spare).tags["shard_id"] != "s-rec" {
		t.Fatal("claimed spare not stamped with its shard, so a replayed launch couldn't adopt it")
	}
}

// A fresh mint claims the spare and gives it a new volume.
func TestEC2Provisioner_MintClaimsASpare(t *testing.T) {
	vols := &swapVolumes{shardID: "s-other"}
	p, f, spare := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-new", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != spare || len(f.insts) != before {
		t.Fatalf("mint landed on %s with %d new instances, want the spare %s and none", res.InstanceID, len(f.insts)-before, spare)
	}
	if vols.where() != spare {
		t.Fatalf("fresh volume attached to %q, want the spare", vols.where())
	}
}

// With no spare in the AZ, a launch falls back to running a fresh
// instance: the pool only ever shortens a path.
func TestEC2Provisioner_LaunchFallsBackWithoutASpare(t *testing.T) {
	vols := &swapVolumes{shardID: "s-other"}
	p, f, spare := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-b", Tier: "small", AZ: "us-east-2b", SizeGB: 20})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID == spare || len(f.insts) != before+1 {
		t.Fatalf("launch in an AZ without a spare: instance %s, %d new", res.InstanceID, len(f.insts)-before)
	}
}

func TestEC2Provisioner_LaunchSpareRejectsAnUnknownPair(t *testing.T) {
	p := spareProvisioner(newFakeEC2())
	if _, err := p.LaunchSpare(context.Background(), "small", "us-west-1a", ""); err == nil {
		t.Fatal("a spare in an AZ the tier has no subnet for must fail")
	}
	if _, err := p.LaunchSpare(context.Background(), "huge", "us-east-2a", ""); err == nil {
		t.Fatal("a spare for an unconfigured tier must fail")
	}
}
