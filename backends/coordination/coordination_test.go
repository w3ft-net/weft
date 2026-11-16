package coordination

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logtest"
)

type fakeLeases struct{ held []string }

func (f fakeLeases) AcquireLease(string, string, time.Duration) (bool, error) { return true, nil }
func (f fakeLeases) ReleaseLease(string, string) error                        { return nil }
func (f fakeLeases) HeldLeases(context.Context, string) ([]string, error)     { return f.held, nil }

// One lease_state line per lease the holder holds, so a cron can read who
// holds what without a lease having changed hands in its window.
func TestLogHeldLeases_OneLinePerLease(t *testing.T) {
	out := logtest.Capture(func() {
		logHeldLeases(context.Background(), fakeLeases{held: []string{"a", "b"}}, "coord-a")
	})
	for _, n := range []string{"a", "b"} {
		if !strings.Contains(out, "lease_state lease="+n+" holder=coord-a") {
			t.Errorf("log = %q, want a lease_state line for %s", out, n)
		}
	}
}

// The first acquire is all AcquireAndHold needs to hand back a live context.
func TestAcquireAndHold_GrantedLeaseGivesALiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaseCtx, ok := AcquireAndHold(ctx, fakeLeases{}, "poller:acme", "coord-a")
	if !ok || leaseCtx.Err() != nil {
		t.Fatalf("ok=%v ctx err=%v, want a live lease context", ok, leaseCtx.Err())
	}
}
