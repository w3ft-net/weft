//go:build test

package logdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The old instance dies before the volume moves: the upgrade can't pause
// a dead instance, so it is abandoned (the claimed standby with it) and the
// shard goes back to its origin state, whose stale check sends it down
// the recovery path like any shard whose instance died.
func TestUpgrade_TheOldInstanceDyingAbandonsTheUpgradeForRecovery(t *testing.T) {
	f := newUpgradeFixture(t, StateReadOnly, false)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // on-demand standby launched, not heartbeating yet
	standby := f.ec2.launched[0]

	f.advance(f.sh.deps.StaleThreshold + time.Minute) // the old instance has gone quiet
	f.evaluate(t)
	if st := f.state(t); st != StateReadOnly || f.phase() != "" {
		t.Fatalf("after the old instance died: %s/%q, want READ_ONLY with the upgrade cleared", st, f.phase())
	}
	if !f.terminated(standby) {
		t.Fatal("the abandoned upgrade's spare was kept")
	}
	f.evaluate(t)
	if st := f.state(t); st != StateSuspended {
		t.Fatalf("next tick: %s, want SUSPENDED (recovery)", st)
	}
}

// The new instance dies after the swap: the shard is on it, so recovery
// handles it; the demoted instance, which nothing would ever terminate
// once the upgrade is gone, is terminated first.
func TestUpgrade_TheNewInstanceDyingAfterTheSwapCleansUpTheDemoted(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.cfg.setReady(standbyAddr)
	f.evaluate(t)
	if f.phase() != PhaseResumed {
		t.Fatalf("phase %q, want resumed", f.phase())
	}
	f.advance(f.sh.deps.StaleThreshold + time.Minute) // the new instance never heartbeats
	f.evaluate(t)
	if !f.terminated(oldInstance) {
		t.Fatal("the demoted instance outlived the upgrade it belonged to")
	}
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("after the new instance died: %s/%q, want INGESTING with the upgrade cleared", st, f.phase())
	}
	if f.sh.InstanceID != "i-spare" {
		t.Fatalf("shard on %s, want i-spare (the instance recovery must replace)", f.sh.InstanceID)
	}
	f.evaluate(t)
	if st := f.state(t); st != StateSuspended {
		t.Fatalf("next tick: %s, want SUSPENDED (recovery)", st)
	}
}

// A rollback whose old instance never resumes (it died while paused) ends
// in recovery rather than holding the shard in UPGRADING forever.
func TestUpgrade_ARollbackThatCannotResumeEndsInRecovery(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	f.pauser.resumeErr = map[string]error{oldAddr: errors.New("connection refused")}
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t)
	f.advance(upgradeSwapBudget + time.Second)
	f.evaluate(t) // → rolling_back
	for i := 0; i < 3; i++ {
		f.evaluate(t) // volume back on the old instance; Resume keeps failing
	}
	if st := f.state(t); st != StateUpgrading {
		t.Fatalf("gave up on resuming immediately: %s", st)
	}
	f.advance(upgradeRollBackBudget + time.Second)
	f.evaluate(t)
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("past the rollback budget: %s/%q, want INGESTING with the upgrade cleared", st, f.phase())
	}
	f.evaluate(t)
	if st := f.state(t); st != StateSuspended {
		t.Fatalf("next tick: %s, want SUSPENDED (recovery)", st)
	}
}

// The old instance dies while the volume is off it (the pause hung on a
// frozen instance, then the instance was terminated): the rollback has
// nothing to reattach the volume to. It retried AttachVolume against the dead
// instance every tick for ever, the shard held in UPGRADING with its reads
// failing and the claimed standby never released (live test S5, 2026-10-27,
// 8 minutes and counting). It must end the upgrade for recovery instead.
func TestUpgrade_ARollbackWhoseOldInstanceIsGoneEndsInRecovery(t *testing.T) {
	f := newUpgradeFixture(t, StateIngesting, true)
	if err := f.sh.startUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.evaluate(t) // swapping; the volume is on the standby

	// Every instance is gone, the volume with them.
	f.inst.mu.Lock()
	f.inst.gone = true
	f.inst.mu.Unlock()
	f.vols.mu.Lock()
	f.vols.attachedTo = ""
	f.vols.attachErr = fmt.Errorf("InvalidInstanceID: %s is not running", oldInstance)
	f.vols.mu.Unlock()

	f.advance(5 * time.Second)
	f.evaluate(t) // → rolling_back
	for i := 0; i < 3; i++ {
		f.advance(5 * time.Second)
		f.evaluate(t)
	}
	if st := f.state(t); st != StateIngesting || f.phase() != "" {
		t.Fatalf("20s into a rollback to a dead instance: %s/%q, want INGESTING with the upgrade cleared (recovery next)", st, f.phase())
	}
	f.advance(f.sh.deps.StaleThreshold + time.Minute) // the dead instance stays silent
	f.evaluate(t)
	if st := f.state(t); st != StateSuspended {
		t.Fatalf("next tick: %s, want SUSPENDED (recovery)", st)
	}
}
