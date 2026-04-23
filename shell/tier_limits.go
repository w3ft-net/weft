package shell

import (
	"time"

	"github.com/kevlarcon/weft/store"
)

// storeTierLimits adapts store's tier helpers to the
// shellcore.TierLimitsProvider interface. Lives in shell/ (not
// shellcore/) so shellcore stays independent of the database layer
// — the same core is reused by the WASM frontend which has no
// store dependency.
type storeTierLimits struct{}

func (storeTierLimits) WatchLimit(tier string) int { return store.WatchLimit(tier) }

func (storeTierLimits) MinWatchInterval(tier string) time.Duration {
	return store.MinWatchInterval(tier)
}

// Tiers lists every plan name help cron should render. Kept here
// rather than store/ because the enumeration is a presentation
// concern — store/ only answers questions per-tier.
func (storeTierLimits) Tiers() []string {
	return []string{"trial", "swatch", "yard", "bolt"}
}
