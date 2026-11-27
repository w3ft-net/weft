package mem

import (
	"context"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/coordination"
)

// Coordination is a coordination.Coordination for one process: every lease is
// granted, and cron slot claims are kept in memory.
type Coordination struct {
	mu     sync.Mutex
	claims map[string]time.Time
}

var _ coordination.Coordination = (*Coordination)(nil)

// NewCoordination is an empty one.
func NewCoordination() *Coordination { return &Coordination{claims: map[string]time.Time{}} }

func (*Coordination) AcquireLease(string, string, time.Duration) (bool, error) { return true, nil }
func (*Coordination) ReleaseLease(string, string) error                        { return nil }
func (*Coordination) HeldLeases(context.Context, string) ([]string, error)     { return nil, nil }

func (c *Coordination) ClaimCronSlot(key, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.claims[key]; taken {
		return false, nil
	}
	c.claims[key] = time.Now()
	return true, nil
}

func (c *Coordination) DeleteCronClaimsBefore(cutoff time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for k, at := range c.claims {
		if at.Before(cutoff) {
			delete(c.claims, k)
			n++
		}
	}
	return n, nil
}
