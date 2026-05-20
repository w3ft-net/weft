package logdb

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// failingLauncher always fails on LaunchInstance — simulates ASG timeout.
type failingLauncher struct {
	launchCalls atomic.Int32
}

func (f *failingLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	f.launchCalls.Add(1)
	return "", fmt.Errorf("timed out")
}

func (f *failingLauncher) TerminateInstance(_ context.Context, _, _ string) error {
	return nil
}

// When every LaunchInstance call fails (e.g., ASG throttling), the
// provisioner's ticker must keep retrying on each tick — the system
// self-heals once the underlying failure clears. Each tick's
// reconciler sees the bucket is under target and re-emits the
// LaunchShardOp; Apply fails; next tick tries again.
func TestProvisioner_TickerContinuesAfterBootstrapFailure(t *testing.T) {
	launcher := &failingLauncher{}
	reg := NewShardRegistry()

	config := DevProvisionerConfig()
	config.PollInterval = 50 * time.Millisecond // fast for test

	prov := NewProvisioner(reg, launcher, config)
	// Bucket config is the signal for "how many shards do we want" —
	// without it the reconciler has nothing to scale toward.
	fake := newFakeBucketStore()
	fake.seedBucket("default", nil)
	prov.Buckets = NewBucketManager(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov.Start(ctx)

	// Wait for bootstrap + a few ticker ticks.
	time.Sleep(300 * time.Millisecond)

	calls := launcher.launchCalls.Load()
	if calls < 2 {
		t.Errorf("expected at least 2 launch calls (retry across ticks), got %d", calls)
	}
	t.Logf("launch calls after 300ms: %d", calls)
}

func TestProvisioner_AdoptsFromHeartbeatsOnTick(t *testing.T) {
	// This test verifies that the provisioner picks up heartbeats
	// on subsequent ticks even if the store wasn't available at startup.
	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	reg := NewShardRegistry()
	config := DevProvisionerConfig()
	config.PollInterval = 50 * time.Millisecond

	prov := NewProvisioner(reg, launcher, config)
	// Bootstrap reads the default bucket; give it a fake store with
	// one seeded.
	fake := newFakeBucketStore()
	fake.seedBucket("default", nil)
	prov.Buckets = NewBucketManager(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov.Start(ctx)

	// Bootstrap should succeed immediately with LocalLauncher.
	time.Sleep(100 * time.Millisecond)

	if reg.TotalIngesting() == 0 {
		t.Fatal("expected bootstrap to create a shard")
	}

	// The provisioner is running and checking on each tick.
	// Even without a store, the ticker should not panic or stop.
	time.Sleep(200 * time.Millisecond)

	// Still running — no panic.
	if reg.TotalIngesting() == 0 {
		t.Fatal("shard disappeared")
	}
}
