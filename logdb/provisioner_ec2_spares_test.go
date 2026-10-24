package logdb

import (
	"context"
	"testing"
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

func TestEC2Provisioner_LaunchSpareRejectsAnUnknownPair(t *testing.T) {
	p := spareProvisioner(newFakeEC2())
	if _, err := p.LaunchSpare(context.Background(), "small", "us-west-1a", ""); err == nil {
		t.Fatal("a spare in an AZ the tier has no subnet for must fail")
	}
	if _, err := p.LaunchSpare(context.Background(), "huge", "us-east-2a", ""); err == nil {
		t.Fatal("a spare for an unconfigured tier must fail")
	}
}
