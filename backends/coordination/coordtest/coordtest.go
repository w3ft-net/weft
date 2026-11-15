// Package coordtest is the conformance suite for coordination.Coordination: the
// behaviour every implementation must show, whatever it stores claims in. An
// implementation's own test file calls Run with a constructor. It is the first
// control-plane suite of docs/plans/tenant-scope.md, Phase 6, and the pattern
// the Phase 5 harness (tenancy/conformance) follows.
//
// Claims are the same contract everywhere. Leases are two contracts, and the
// subject says which it implements: an exclusive lease is held by one holder
// until it releases or expires; an always-held lease is granted to every
// asker, which is right for a build with one coordinator.
package coordtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
)

// Subject is one implementation under test, fresh for one subtest.
type Subject struct {
	C coordination.Coordination

	// ExclusiveLeases says a lease is held by one holder at a time and expires.
	// False says every acquire is granted and HeldLeases reports none.
	ExclusiveLeases bool

	// Backdate moves a claim's recorded time age into the past.
	Backdate func(key string, age time.Duration) error

	// Reopen returns a coordination over the state this one persisted, as a
	// restarted process sees it.
	Reopen func() (coordination.Coordination, error)

	// Forget removes whatever the subtest left under the key or name prefix,
	// for an implementation whose storage outlives the subject. Optional.
	Forget func(prefix string)
}

// Run runs the suite. newSubject is called once per subtest.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Helper()
	sub := func(name string, f func(t *testing.T, s Subject, p string)) {
		t.Run(name, func(t *testing.T) {
			s := newSubject(t)
			p := fmt.Sprintf("coordtest:%d:", time.Now().UnixNano())
			if s.Forget != nil {
				t.Cleanup(func() { s.Forget(p) })
			}
			f(t, s, p)
		})
	}

	sub("Claims/ExactlyOneWinnerUnderConcurrency", func(t *testing.T, s Subject, p string) {
		key := p + "slot"
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				won, err := s.C.ClaimCronSlot(key, fmt.Sprintf("coord-%d", i))
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if won {
					wins.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("%d callers won one key, want exactly 1", got)
		}
	})

	sub("Claims/DistinctKeysAllWin", func(t *testing.T, s Subject, p string) {
		for i := 0; i < 5; i++ {
			won, err := s.C.ClaimCronSlot(fmt.Sprintf("%sslot-%d", p, i), "coord-a")
			if err != nil || !won {
				t.Fatalf("key %d: won=%v err=%v, want a win", i, won, err)
			}
		}
	})

	sub("Claims/SameHolderCannotClaimTwice", func(t *testing.T, s Subject, p string) {
		key := p + "slot"
		if won, err := s.C.ClaimCronSlot(key, "coord-a"); err != nil || !won {
			t.Fatalf("first claim: won=%v err=%v", won, err)
		}
		if won, err := s.C.ClaimCronSlot(key, "coord-a"); err != nil || won {
			t.Fatalf("second claim by the same holder: won=%v err=%v, want a loss", won, err)
		}
	})

	sub("Claims/SurviveReopen", func(t *testing.T, s Subject, p string) {
		key := p + "slot"
		if won, err := s.C.ClaimCronSlot(key, "coord-a"); err != nil || !won {
			t.Fatalf("claim: won=%v err=%v", won, err)
		}
		re, err := s.Reopen()
		if err != nil {
			t.Fatal(err)
		}
		if won, err := re.ClaimCronSlot(key, "coord-b"); err != nil || won {
			t.Fatalf("claim after a restart: won=%v err=%v, want a loss: the slot already ran", won, err)
		}
	})

	sub("Claims/DeleteBeforeRemovesOnlyOlder", func(t *testing.T, s Subject, p string) {
		old, fresh := p+"old", p+"fresh"
		for _, k := range []string{old, fresh} {
			if won, err := s.C.ClaimCronSlot(k, "me"); err != nil || !won {
				t.Fatalf("claim %s: won=%v err=%v", k, won, err)
			}
		}
		if err := s.Backdate(old, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		n, err := s.C.DeleteCronClaimsBefore(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// The hosted table is shared by every test of its database, so count
		// at least ours: only the open file is ours alone.
		if n < 1 {
			t.Fatalf("deleted %d claims, want at least the old one", n)
		}
		if won, _ := s.C.ClaimCronSlot(old, "me"); !won {
			t.Error("the old claim was not deleted")
		}
		if won, _ := s.C.ClaimCronSlot(fresh, "me"); won {
			t.Error("the fresh claim was deleted")
		}
	})

	sub("Leases/AcquireReleaseReacquire", func(t *testing.T, s Subject, p string) {
		name := p + "lease"
		if ok, err := s.C.AcquireLease(name, "coord-a", time.Minute); err != nil || !ok {
			t.Fatalf("acquire: ok=%v err=%v", ok, err)
		}
		if ok, err := s.C.AcquireLease(name, "coord-a", time.Minute); err != nil || !ok {
			t.Fatalf("renewal by the holder: ok=%v err=%v", ok, err)
		}
		if err := s.C.ReleaseLease(name, "coord-a"); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.C.AcquireLease(name, "coord-a", time.Minute); err != nil || !ok {
			t.Fatalf("re-acquire after a release: ok=%v err=%v", ok, err)
		}
	})

	sub("Leases/Exclusive", func(t *testing.T, s Subject, p string) {
		if !s.ExclusiveLeases {
			t.Skip("always-held leases")
		}
		name := p + "lease"
		if ok, err := s.C.AcquireLease(name, "coord-a", time.Minute); err != nil || !ok {
			t.Fatalf("acquire: ok=%v err=%v", ok, err)
		}
		if ok, err := s.C.AcquireLease(name, "coord-b", time.Minute); err != nil || ok {
			t.Fatalf("another holder acquired a held lease: ok=%v err=%v", ok, err)
		}
		if err := s.C.ReleaseLease(name, "coord-b"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.C.AcquireLease(name, "coord-b", time.Minute); ok {
			t.Fatal("a release by a non-holder freed the lease")
		}
		if err := s.C.ReleaseLease(name, "coord-a"); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.C.AcquireLease(name, "coord-b", time.Minute); err != nil || !ok {
			t.Fatalf("acquire after the holder released: ok=%v err=%v", ok, err)
		}
	})

	sub("Leases/ExclusiveExpiryTakeover", func(t *testing.T, s Subject, p string) {
		if !s.ExclusiveLeases {
			t.Skip("always-held leases")
		}
		name := p + "lease"
		if ok, err := s.C.AcquireLease(name, "coord-a", -time.Second); err != nil || !ok {
			t.Fatalf("acquire with a lapsed ttl: ok=%v err=%v", ok, err)
		}
		if ok, err := s.C.AcquireLease(name, "coord-b", time.Minute); err != nil || !ok {
			t.Fatalf("takeover of an expired lease: ok=%v err=%v", ok, err)
		}
	})

	sub("Leases/ExclusiveHeldLeases", func(t *testing.T, s Subject, p string) {
		if !s.ExclusiveLeases {
			t.Skip("always-held leases")
		}
		for _, l := range []struct {
			name, holder string
			ttl          time.Duration
		}{
			{p + "held", "coord-a", time.Minute},
			{p + "expired", "coord-a", -time.Minute},
			{p + "other", "coord-b", time.Minute},
		} {
			if _, err := s.C.AcquireLease(l.name, l.holder, l.ttl); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.C.HeldLeases(context.Background(), "coord-a")
		if err != nil {
			t.Fatal(err)
		}
		var ours []string
		for _, n := range got {
			if len(n) >= len(p) && n[:len(p)] == p {
				ours = append(ours, n)
			}
		}
		if len(ours) != 1 || ours[0] != p+"held" {
			t.Errorf("HeldLeases = %v, want only %sheld", ours, p)
		}
	})

	sub("Leases/AlwaysHeld", func(t *testing.T, s Subject, p string) {
		if s.ExclusiveLeases {
			t.Skip("exclusive leases")
		}
		name := p + "lease"
		for _, h := range []string{"coord-a", "coord-b"} {
			if ok, err := s.C.AcquireLease(name, h, time.Minute); err != nil || !ok {
				t.Fatalf("%s: ok=%v err=%v, want every acquire granted", h, ok, err)
			}
		}
		if got, err := s.C.HeldLeases(context.Background(), "coord-a"); err != nil || len(got) != 0 {
			t.Errorf("HeldLeases = %v, %v, want none: the open build keeps no record", got, err)
		}
	})
}
