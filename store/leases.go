package store

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"time"
)

// LeaseTTL is how long an acquired lease stays valid without renewal.
// Bounds failover time: a dead holder's lease is reclaimable within this
// long. LeaseRenewInterval is how often a live holder renews — a third
// of the TTL so one missed tick (GC pause, transient DB blip) doesn't
// cost the lease.
const (
	LeaseTTL           = 30 * time.Second
	LeaseRenewInterval = LeaseTTL / 3
)

// AcquireLease attempts to acquire or renew the named lease for holder.
// Returns true if the caller now holds it: freshly acquired, renewed
// because it already held it, or taken over from a holder whose lease
// had expired. Returns false if someone else currently holds a
// non-expired lease — the caller does not hold it and must not act as
// if it does.
//
// Single atomic statement, so there's no read-then-write race window
// between checking who holds the lease and claiming it.
func (s *Store) AcquireLease(name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	res := s.db.Exec(`
		INSERT INTO leases (name, holder, expires_at, acquired_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			holder = EXCLUDED.holder,
			expires_at = EXCLUDED.expires_at,
			acquired_at = CASE WHEN leases.holder = EXCLUDED.holder
				THEN leases.acquired_at
				ELSE EXCLUDED.acquired_at
			END
		WHERE leases.holder = ? OR leases.expires_at < ?`,
		name, holder, now.Add(ttl), now, holder, now)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ReleaseLease gives up a lease this holder currently owns. Best-effort
// and optional to call — an unreleased lease simply expires on its own,
// so a crash or missed release is never a correctness problem, only a
// slightly slower handoff.
func (s *Store) ReleaseLease(name, holder string) error {
	return s.db.Exec(`DELETE FROM leases WHERE name = ? AND holder = ?`, name, holder).Error
}

// LogLeaseEvent emits one structured line per lease-state transition
// (acquired, denied, lost, released, deadman_released) so a coordinator
// dashboard/cron can see "who's leading what" over time without querying
// Postgres directly. All 5 uses of the lease primitive (NAT,
// shard-fsm-leader, per-poller, per-cron — see
// docs/plans/coordinator-horizontal-scaling.md) route through this so the
// log shape stays consistent. Deliberately excludes routine renewals:
// those are the expected steady state and would dwarf the interesting
// transitions in log volume.
func LogLeaseEvent(name, holder, event string) {
	weftsdk.Infof(context.Background(), "lease_event lease=%s holder=%s event=%s", name, holder, event)
}

// LogLeaseStates logs a lease_state line for each lease holder holds, every
// interval until ctx ends. lease_event lines record only transitions, so a
// cron whose window saw none can't tell from them who holds what; these
// state lines can.
func (s *Store) LogLeaseStates(ctx context.Context, holder string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.logHeldLeases(ctx, holder)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// logHeldLeases logs one lease_state line per unexpired lease holder holds.
func (s *Store) logHeldLeases(ctx context.Context, holder string) {
	var names []string
	if err := s.db.WithContext(ctx).Raw(
		`SELECT name FROM leases WHERE holder = ? AND expires_at > ? ORDER BY name`,
		holder, time.Now().UTC()).Scan(&names).Error; err != nil {
		weftsdk.Warnf(ctx, "lease_state: listing leases held by %s: %v", holder, err)
		return
	}
	for _, n := range names {
		weftsdk.Infof(ctx, "lease_state lease=%s holder=%s", n, holder)
	}
}

// AcquireAndHold tries once to acquire the named lease for holder. On
// success it starts a background goroutine that renews the lease every
// LeaseRenewInterval and returns a context derived from ctx that is
// canceled the moment renewal ever fails for any reason (lost to
// another holder, or a DB error) — fail closed, since the alternative is
// two instances both believing they hold the same lease. Callers should
// select on the returned context alongside their own work loop and stop
// as soon as it's done.
//
// Returns ok=false, doing nothing, if the initial acquire fails —
// callers should not start the guarded work at all in that case.
func AcquireAndHold(ctx context.Context, s *Store, name, holder string) (leaseCtx context.Context, ok bool) {
	acquired, err := s.AcquireLease(name, holder, LeaseTTL)
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
				renewed, err := s.AcquireLease(name, holder, LeaseTTL)
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
