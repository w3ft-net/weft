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

// shellcore's TierLimitsProvider interface is string-typed (it runs
// from WASM and must not take a dep on the store package). The two
// methods below bridge that string boundary to the typed store.Tier
// API by wrapping the caller's string in store.Tier at the door.
func (storeTierLimits) WatchLimit(tier string) int { return store.WatchLimit(store.Tier(tier)) }

func (storeTierLimits) MinWatchInterval(tier string) time.Duration {
	return store.MinWatchInterval(store.Tier(tier))
}

// Tiers lists every plan name help cron should render. Kept here
// rather than store/ because the enumeration is a presentation
// concern — store/ only answers questions per-tier.
func (storeTierLimits) Tiers() []string {
	return []string{
		string(store.TierTrial),
		string(store.TierSwatch),
		string(store.TierYard),
		string(store.TierBolt),
	}
}
