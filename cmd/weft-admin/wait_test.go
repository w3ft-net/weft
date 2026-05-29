package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// pollShardReady is the wait-and-verify primitive the cohort/fleet rollers
// use after firing RelaunchShard: poll the coordinator for a shard's
// state+heartbeat until it's promoted back to INGESTING with a heartbeat
// stamped AFTER the relaunch cutoff. The tests use a fake getter so we
// can drive the state machine deterministically without standing up a
// real coordinator.

type fakeGetter struct {
	calls  int
	states []string    // returned in order; last value sticks if calls exceeds len
	hb     []time.Time // parallel slice
	shas   []string    // parallel slice; empty when the test doesn't care
	err    error       // returned every call when non-nil
}

func (f *fakeGetter) get(_ string) (string, time.Time, string, error) {
	if f.err != nil {
		f.calls++
		return "", time.Time{}, "", f.err
	}
	i := f.calls
	if i >= len(f.states) {
		i = len(f.states) - 1
	}
	f.calls++
	sha := ""
	if i < len(f.shas) {
		sha = f.shas[i]
	}
	return f.states[i], f.hb[i], sha, nil
}

func TestPollShardReady_SucceedsImmediately(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		states: []string{"INGESTING"},
		hb:     []time.Time{cutoff.Add(time.Second)}, // post-cutoff
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pollShardReady(ctx, f.get, "s-1", cutoff, "", 5*time.Millisecond); err != nil {
		t.Fatalf("expected nil (INGESTING + post-cutoff heartbeat), got %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("expected single poll, got %d", f.calls)
	}
}

func TestPollShardReady_SucceedsAfterCycle(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		states: []string{"SUSPENDED", "LAUNCHING", "LAUNCHING", "INGESTING"},
		hb:     []time.Time{cutoff.Add(-time.Hour), {}, {}, cutoff.Add(time.Second)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pollShardReady(ctx, f.get, "s-1", cutoff, "", time.Millisecond); err != nil {
		t.Fatalf("expected nil after the shard cycled back, got %v", err)
	}
	if f.calls != 4 {
		t.Fatalf("expected 4 polls (one per state in sequence), got %d", f.calls)
	}
}

// The row briefly reads INGESTING + a recent (pre-relaunch) heartbeat the
// instant after RelaunchShard fires — before the FSM has picked up the
// trigger. The post-cutoff check must reject that and keep waiting.
func TestPollShardReady_RejectsPreCutoffHeartbeat(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		// First call: stale pre-relaunch INGESTING (must NOT succeed).
		// Then the cycle, ending in a fresh post-cutoff INGESTING.
		states: []string{"INGESTING", "SUSPENDED", "LAUNCHING", "INGESTING"},
		hb:     []time.Time{cutoff.Add(-30 * time.Second), {}, {}, cutoff.Add(time.Second)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pollShardReady(ctx, f.get, "s-1", cutoff, "", time.Millisecond); err != nil {
		t.Fatalf("expected success on the post-cycle fresh INGESTING, got %v", err)
	}
	if f.calls != 4 {
		t.Fatalf("must skip the pre-cutoff INGESTING and only succeed on the post-cycle one; got %d polls", f.calls)
	}
}

func TestPollShardReady_TimesOut(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		states: []string{"SUSPENDED"}, // stuck forever
		hb:     []time.Time{cutoff.Add(-time.Hour)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := pollShardReady(ctx, f.get, "s-1", cutoff, "", 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestPollShardReady_GetterError(t *testing.T) {
	f := &fakeGetter{err: fmt.Errorf("rpc unavailable")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := pollShardReady(ctx, f.get, "s-1", time.Now(), "", 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected getter error to surface, got nil")
	}
}

// When wantSHA is set, the post-relaunch shard must report that exact SHA
// before the poll succeeds — otherwise a shard that came up on the OLD
// binary (e.g. the new instance failed to fetch the new AMI) would silently
// pass.
func TestPollShardReady_WantSHARequiresMatch(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		// Cycle: SUSPENDED → LAUNCHING → INGESTING(old SHA — must NOT match)
		// → INGESTING(target SHA — succeeds).
		states: []string{"SUSPENDED", "LAUNCHING", "INGESTING", "INGESTING"},
		hb: []time.Time{
			cutoff.Add(-time.Hour),
			{},
			cutoff.Add(time.Second),
			cutoff.Add(2 * time.Second),
		},
		shas: []string{"", "", "oldsha", "newsha"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pollShardReady(ctx, f.get, "s-1", cutoff, "newsha", time.Millisecond); err != nil {
		t.Fatalf("expected success on the post-cycle matching-SHA INGESTING, got %v", err)
	}
	if f.calls != 4 {
		t.Fatalf("must reject INGESTING with the old SHA and only succeed when the SHA matches; got %d polls", f.calls)
	}
}

func TestPollShardReady_EmptyWantSHASkipsCheck(t *testing.T) {
	cutoff := time.Now()
	f := &fakeGetter{
		states: []string{"INGESTING"},
		hb:     []time.Time{cutoff.Add(time.Second)},
		shas:   []string{"anything"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pollShardReady(ctx, f.get, "s-1", cutoff, "", time.Millisecond); err != nil {
		t.Fatalf("empty wantSHA must skip the SHA check, got %v", err)
	}
}
