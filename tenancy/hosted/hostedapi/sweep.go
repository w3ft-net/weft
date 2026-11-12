package hostedapi

import (
	"context"
	"time"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
	weftsdk "github.com/w3ft-net/weft-go"
)

const unverifiedSweepInterval = 1 * time.Hour

// StartUnverifiedSweep starts a background goroutine that periodically
// deletes accounts that were created but never verified within the grace
// period. Blocks until ctx is cancelled. Cascading FK deletes take care
// of the user's customer/subscription/team-member/tokens/sources.
func (p *product) StartUnverifiedSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(unverifiedSweepInterval)
		defer ticker.Stop()
		// Run once at startup so restarts don't give squatters a free
		// extra interval. Ignore errors — we'll retry next tick.
		p.sweepUnverified(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.sweepUnverified(ctx)
			}
		}
	}()
}

func (p *product) sweepUnverified(ctx context.Context) {
	cutoff := time.Now().Add(-auth.UnverifiedGracePeriod)
	result := p.Store().DB().
		Where("email_verified_at IS NULL AND created_at < ?", cutoff).
		Delete(&store.User{})
	n, err := result.RowsAffected, result.Error
	if err != nil {
		weftsdk.Infof(ctx, "unverified sweep: %v", err)
		return
	}
	if n > 0 {
		weftsdk.Infof(ctx, "unverified sweep: deleted %d expired accounts", n)
	}
}
