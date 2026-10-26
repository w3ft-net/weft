//go:build test

package logdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// A leader that was stopped for longer than its lease lasts still has its
// flag set when it wakes: the ticker that clears it hasn't run yet. The lease's
// own expiry, which the wall clock has already passed, is what refuses it.
func TestLeaseValid_ExpiresWithoutATick(t *testing.T) {
	p := &Provisioner{}
	p.electing.Store(true)
	p.leading.Store(true)
	p.leaseUntil.Store(time.Now().Add(store.LeaseTTL).UnixNano())
	if !p.leaseValid() {
		t.Fatal("a freshly renewed lease reads as expired")
	}

	p.leaseUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if p.leaseValid() {
		t.Fatal("a lease past its expiry still reads as valid; the flag alone was trusted")
	}

	// Close to expiry counts as expired: the lease row's clock is the
	// database's, not ours.
	p.leaseUntil.Store(time.Now().Add(leaseSafetyMargin / 2).UnixNano())
	if p.leaseValid() {
		t.Fatal("a lease within the safety margin of its expiry reads as valid")
	}

	p.leaseUntil.Store(time.Now().Add(store.LeaseTTL).UnixNano())
	p.leading.Store(false)
	if p.leaseValid() {
		t.Fatal("a process that lost the lease reads as leading")
	}
}

// With no election running (dev, tests) the process is the sole, permanent
// leader and nothing expires.
func TestLeaseValid_NoElectionIsAlwaysValid(t *testing.T) {
	p := &Provisioner{}
	p.leading.Store(true)
	if !p.leaseValid() {
		t.Fatal("a process with no election read as not leading")
	}
}

// The provisioner hands every shard the lease check, and the fenced deps built
// from it refuse a leader's call once the lease has expired.
func TestShardDeps_AreFencedByTheProvisionersLease(t *testing.T) {
	p := NewProvisioner(NewShardRegistry(), nil, ProvisionerConfig{PollInterval: 5 * time.Second})
	p.volumes = &fakeVolumeManager{}
	d := p.shardDeps()
	if d.Leading == nil {
		t.Fatal("the provisioner's shard deps carry no lease check")
	}
	if _, fenced := d.Volumes.(fencedVolumes); !fenced {
		t.Fatalf("volumes are a %T, want them fenced", d.Volumes)
	}

	p.electing.Store(true)
	p.leading.Store(true)
	p.leaseUntil.Store(time.Now().Add(-time.Second).UnixNano())
	ctx := withLeaderFence(context.Background(), d.Leading)
	if err := d.Volumes.AttachToInstance(ctx, "vol-1", "i-1", "/dev/sdf"); !errors.Is(err, errLostLease) {
		t.Fatalf("attach after the lease expired returned %v, want errLostLease", err)
	}
}
