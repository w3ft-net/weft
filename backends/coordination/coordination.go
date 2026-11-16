// Package coordination is how one coordinator knows it may act: which process
// leads, and which scheduled slot it alone may run. It is the first
// control-plane interface. Nothing in it is
// group-scoped, so no call takes a partition.
//
// The interface keeps two kinds of thing apart, because a build with one
// coordinator needs only one of them:
//
//   - Ownership (Leases): a name that at most one holder may act under at a
//     time. A build with one coordinator holds every lease by definition, so
//     its implementation (defaults) answers true and keeps no state.
//     A fleet needs the lease to be exclusive and to expire; the hosted
//     implementation is a Postgres table with a TTL.
//   - State with claim semantics (Claims): a key that exactly one caller ever
//     wins. This is idempotency, not mutual exclusion: a coordinator that
//     restarts inside a cron's slot must not fire the slot again, with or
//     without a second node. Every implementation therefore persists claims.
//
// The atomicity of a claim is the single statement that records it (insert,
// do nothing on conflict, read the rows affected). Nothing here takes a lock.
package coordination

import (
	"context"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// LeaseTTL is how long an acquired lease stays valid without renewal. It
// bounds failover: a dead holder's lease is reclaimable within this long.
// LeaseRenewInterval is how often a live holder renews, a third of the TTL so
// one missed tick (a GC pause, a transient DB blip) doesn't cost the lease.
const (
	LeaseTTL           = 30 * time.Second
	LeaseRenewInterval = LeaseTTL / 3
)

// Leases is ownership of named roles.
//
// Contract: a lease's expiry is decided by the backend's clock, never by a
// caller's. Two coordinators whose clocks differ must still agree on who holds
// a lease. (Postgres: clock_timestamp(); etcd and Consul: server-side TTL.) A
// backend that can only compare caller-supplied timestamps must not claim to
// implement Leases.
type Leases interface {
	// AcquireLease acquires or renews the named lease for holder and reports
	// whether holder now holds it: freshly acquired, renewed because it
	// already held it, or taken over from a holder whose lease had expired.
	// False means someone else holds an unexpired lease, and the caller must
	// not act as if it does. An error means the state is unknown.
	AcquireLease(name, holder string, ttl time.Duration) (bool, error)
	// ReleaseLease gives up a lease holder owns. It is best effort and
	// optional: an unreleased lease expires on its own, so a missed release
	// costs only a slower handoff.
	ReleaseLease(name, holder string) error
	// HeldLeases is the names of the unexpired leases holder holds, for
	// reporting. An implementation that holds everything and keeps no record
	// (the open one) returns none.
	HeldLeases(ctx context.Context, holder string) ([]string, error)
}

// Claims is exactly-once state keyed by an opaque string.
type Claims interface {
	// ClaimCronSlot claims a slot key for holder. Exactly one caller per key
	// ever gets true, across restarts; everyone else gets false. An error
	// means the claim state is unknown and the caller must not run the slot.
	ClaimCronSlot(key, holder string) (won bool, err error)
	// DeleteCronClaimsBefore removes claims older than cutoff and returns how
	// many were removed.
	DeleteCronClaimsBefore(cutoff time.Time) (int64, error)
}

// Coordination is both halves, what a coordinator process is handed.
type Coordination interface {
	Leases
	Claims
}

// LogLeaseEvent emits one structured line per lease-state transition
// (acquired, denied, lost, released, deadman_released) so a dashboard or cron
// can see who is leading what over time. Routine renewals are deliberately
// left out: they are the steady state and would dwarf the transitions.
func LogLeaseEvent(name, holder, event string) {
	weftsdk.Infof(context.Background(), "lease_event lease=%s holder=%s event=%s", name, holder, event)
}

// LogLeaseStates logs a lease_state line for each lease holder holds, every
// interval until ctx ends. lease_event lines record only transitions, so a
// cron whose window saw none can't tell from them who holds what; these state
// lines can.
func LogLeaseStates(ctx context.Context, l Leases, holder string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		logHeldLeases(ctx, l, holder)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// logHeldLeases logs one lease_state line per unexpired lease holder holds.
func logHeldLeases(ctx context.Context, l Leases, holder string) {
	names, err := l.HeldLeases(ctx, holder)
	if err != nil {
		weftsdk.Warnf(ctx, "lease_state: listing leases held by %s: %v", holder, err)
		return
	}
	for _, n := range names {
		weftsdk.Infof(ctx, "lease_state lease=%s holder=%s", n, holder)
	}
}

// AcquireAndHold tries once to acquire the named lease for holder. On success
// it starts a goroutine that renews the lease every LeaseRenewInterval and
// returns a context derived from ctx that is canceled the moment renewal ever
// fails for any reason (lost to another holder, or a DB error): fail closed,
// since the alternative is two instances both believing they hold the same
// lease. Callers should select on the returned context alongside their own
// loop and stop as soon as it's done.
//
// It returns ok=false, doing nothing, if the initial acquire fails; callers
// should not start the guarded work at all then.
func AcquireAndHold(ctx context.Context, l Leases, name, holder string) (leaseCtx context.Context, ok bool) {
	acquired, err := l.AcquireLease(name, holder, LeaseTTL)
	if err != nil {
		weftsdk.Errorf(ctx, "lease[%s]: acquire error: %v", name, err)
		return nil, false
	}
	if !acquired {
		LogLeaseEvent(name, holder, "denied")
		return nil, false
	}
	LogLeaseEvent(name, holder, "acquired")

	leaseCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		ticker := time.NewTicker(LeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewed, err := l.AcquireLease(name, holder, LeaseTTL)
				if err != nil {
					weftsdk.Errorf(ctx, "lease[%s]: renew error, giving it up: %v", name, err)
					LogLeaseEvent(name, holder, "lost")
					return
				}
				if !renewed {
					weftsdk.Infof(ctx, "lease[%s]: lost to another holder", name)
					LogLeaseEvent(name, holder, "lost")
					return
				}
			}
		}
	}()
	return leaseCtx, true
}
