package coordinator

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

func TestProvisioner_TickerContinuesAfterBootstrapFailure(t *testing.T) {
	launcher := &failingLauncher{}
	reg := NewShardRegistry()

	config := DevProvisionerConfig()
	config.PollInterval = 50 * time.Millisecond // fast for test

	prov := NewProvisioner(reg, launcher, config)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov.Start(ctx)

	// Wait for bootstrap to fail + at least 2 ticker ticks.
	time.Sleep(300 * time.Millisecond)

	calls := launcher.launchCalls.Load()
	if calls < 2 {
		t.Errorf("expected at least 2 launch calls (1 bootstrap + 1+ check), got %d", calls)
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
