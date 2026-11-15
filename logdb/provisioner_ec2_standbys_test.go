//go:build test

package logdb

import (
	"context"
	"testing"
	"time"
)

func standbyProvisioner(f *fakeEC2) *EC2Provisioner {
	p := newTestEC2Provisioner(f, &fakeVolumeManager{})
	p.imageID = "ami-current"
	return p
}

// A pool standby is a shard instance from the tier's template, in the AZ's
// subnet, on the current image, tagged so it boots in standby mode and so
// the shard paths (findLiveByShard, instance correlation) never see it.
func TestEC2Provisioner_LaunchStandbyTagsAPoolInstance(t *testing.T) {
	f := newFakeEC2()
	p := standbyProvisioner(f)
	in, err := p.LaunchStandby(context.Background(), "small", "us-east-2b", "")
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

// An on-demand standby is born claimed, so a leader that dies before
// recording it leaves an instance the next leader re-derives the claim
// from.
func TestEC2Provisioner_LaunchStandbyForAClaimant(t *testing.T) {
	f := newFakeEC2()
	p := standbyProvisioner(f)
	in, err := p.LaunchStandby(context.Background(), "small", "us-east-2a", "shard-x")
	if err != nil {
		t.Fatal(err)
	}
	if f.find(in.InstanceID).tags["shard_id"] != "shard-x" || in.ClaimedBy != "shard-x" {
		t.Fatalf("on-demand spare not stamped with its claimant: %+v tags=%v", in, f.find(in.InstanceID).tags)
	}
	got, err := p.ListStandbyInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClaimedBy != "shard-x" || got[0].Tier != "small" || got[0].AZ != "us-east-2a" || got[0].ImageID != "ami-current" {
		t.Fatalf("ListSpareInstances: %+v", got)
	}
}

// Claim stamps the claimant; release drops the standby tag, after which the
// instance is the shard's and no longer listed as a standby. Terminated
// standbys aren't listed either.
func TestEC2Provisioner_ClaimReleaseAndListStandbys(t *testing.T) {
	f := newFakeEC2()
	p := standbyProvisioner(f)
	ctx := context.Background()
	a, _ := p.LaunchStandby(ctx, "small", "us-east-2a", "")
	b, _ := p.LaunchStandby(ctx, "small", "us-east-2a", "")
	c, _ := p.LaunchStandby(ctx, "small", "us-east-2b", "")
	if err := p.ClaimInstance(ctx, a.InstanceID, "shard-a"); err != nil {
		t.Fatal(err)
	}
	if f.find(a.InstanceID).tags["shard_id"] != "shard-a" {
		t.Fatal("claim did not stamp shard_id")
	}
	if err := p.ReleaseStandby(ctx, b.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := p.Terminate(ctx, c.InstanceID); err != nil {
		t.Fatal(err)
	}
	got, err := p.ListStandbyInstances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InstanceID != a.InstanceID || got[0].ClaimedBy != "shard-a" {
		t.Fatalf("ListSpareInstances: %+v, want only the claimed %s", got, a.InstanceID)
	}
}

// pooledProvisioner is an EC2Provisioner whose launches claim from a pool
// holding one heartbeating current-image standby in us-east-2a.
func pooledProvisioner(t *testing.T, vols VolumeManager) (*EC2Provisioner, *fakeEC2, string) {
	t.Helper()
	f := newFakeEC2()
	p := newTestEC2Provisioner(f, vols)
	p.imageID = "ami-current"
	db := &memStandbys{}
	pool := NewStandbyPool(db, p, time.Now)
	p.SetStandbys(NewStandbyService(pool, nil, nil))
	if err := pool.Reconcile(context.Background(), map[StandbyPair]bool{{"small", "us-east-2a"}: true}, func(Standby) StandbyUse { return StandbyInUse }); err != nil {
		t.Fatal(err)
	}
	standbys, _ := p.ListStandbyInstances(context.Background())
	if len(standbys) != 1 {
		t.Fatalf("pool seeded %d spares", len(standbys))
	}
	if err := db.ObserveStandbyHeartbeat(standbys[0].InstanceID, "10.0.0.9:9000", "sha", time.Now()); err != nil {
		t.Fatal(err)
	}
	return p, f, standbys[0].InstanceID
}

// A recovery claims the standby instead of booting a fresh instance, and
// force-detaches the volume from the dead instance rather than waiting
// for EC2 to release it.
func TestEC2Provisioner_RecoveryClaimsAStandby(t *testing.T) {
	vols := &swapVolumes{volume: "vol-1", shardID: "s-rec", attachedTo: "i-dead"}
	p, f, standby := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-rec", Tier: "small", AZ: "us-east-2a", ExistingVolumeID: "vol-1"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != standby {
		t.Fatalf("recovery landed on %s, want the claimed spare %s", res.InstanceID, standby)
	}
	if len(f.insts) != before {
		t.Fatal("a recovery with a spare available ran a fresh instance")
	}
	if len(vols.detaches) != 1 || !vols.detaches[0] {
		t.Fatalf("detaches %v: want one forced detach from the dead instance", vols.detaches)
	}
	if vols.where() != standby {
		t.Fatalf("volume attached to %q, want the spare", vols.where())
	}
	if f.find(standby).tags["shard_id"] != "s-rec" {
		t.Fatal("claimed spare not stamped with its shard, so a replayed launch couldn't adopt it")
	}
}

// A fresh mint claims the standby and gives it a new volume.
func TestEC2Provisioner_MintClaimsAStandby(t *testing.T) {
	vols := &swapVolumes{shardID: "s-other"}
	p, f, standby := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-new", Tier: "small", AZ: "us-east-2a", SizeGB: 20})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID != standby || len(f.insts) != before {
		t.Fatalf("mint landed on %s with %d new instances, want the spare %s and none", res.InstanceID, len(f.insts)-before, standby)
	}
	if vols.where() != standby {
		t.Fatalf("fresh volume attached to %q, want the spare", vols.where())
	}
}

// With no standby in the AZ, a launch falls back to running a fresh
// instance: the pool only ever shortens a path.
func TestEC2Provisioner_LaunchFallsBackWithoutAStandby(t *testing.T) {
	vols := &swapVolumes{shardID: "s-other"}
	p, f, standby := pooledProvisioner(t, vols)
	before := len(f.insts)
	res, err := p.Launch(context.Background(), LaunchSpec{ShardID: "s-b", Tier: "small", AZ: "us-east-2b", SizeGB: 20})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.InstanceID == standby || len(f.insts) != before+1 {
		t.Fatalf("launch in an AZ without a spare: instance %s, %d new", res.InstanceID, len(f.insts)-before)
	}
}

func TestEC2Provisioner_LaunchStandbyRejectsAnUnknownPair(t *testing.T) {
	p := standbyProvisioner(newFakeEC2())
	if _, err := p.LaunchStandby(context.Background(), "small", "us-west-1a", ""); err == nil {
		t.Fatal("a spare in an AZ the tier has no subnet for must fail")
	}
	if _, err := p.LaunchStandby(context.Background(), "huge", "us-east-2a", ""); err == nil {
		t.Fatal("a spare for an unconfigured tier must fail")
	}
}
