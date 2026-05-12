package shell

import "time"

// defaultTierLimits mirrors the WatchLimit / MinWatchInterval / tier
// list from store/user.go. Duplicated rather than imported because
// pulling the full store package into this file would drag the entire
// DB layer (pgx, migrations, GORM models, gRPC types) into the WASM
// build through the configureCore import path — a multi-megabyte cost
// for four numbers that change once a year. The store package keeps
// its own copies for the API / coordinator side; if these ever drift
// the configure_core_test will catch the help-corpus diff.
//
// Source of truth is still store/user.go; treat this as a read-only
// mirror.
type defaultTierLimits struct{}

func (defaultTierLimits) WatchLimit(tier string) int {
	switch tier {
	case "swatch":
		return 5
	case "yard":
		return 10
	case "bolt":
		return 50
	default:
		return 1
	}
}

func (defaultTierLimits) MinWatchInterval(tier string) time.Duration {
	switch tier {
	case "swatch", "trial":
		return 5 * time.Minute
	case "yard", "bolt":
		return 1 * time.Minute
	default:
		return 0
	}
}

func (defaultTierLimits) Tiers() []string {
	return []string{"trial", "swatch", "yard", "bolt"}
}
