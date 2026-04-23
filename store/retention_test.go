package store

import (
	"testing"
	"time"
)

// TestLogRetention_Tiers pins the advertised values. If these change,
// the pricing table on the landing page (frontend/src/components/
// PricingTable.tsx) needs to change too, or customers will see a
// number on the marketing page that contradicts what they get.
func TestLogRetention_Tiers(t *testing.T) {
	cases := []struct {
		tier string
		want time.Duration
	}{
		{"trial", 24 * time.Hour},
		{"swatch", 30 * 24 * time.Hour},
		{"yard", 90 * 24 * time.Hour},
		{"bolt", 365 * 24 * time.Hour},
		// Unknown tiers fall back to trial — safer to under-retain
		// an unknown plan than to silently give it Bolt-level access.
		{"", 24 * time.Hour},
		{"garbage", 24 * time.Hour},
	}
	for _, c := range cases {
		got := LogRetention(c.tier)
		if got != c.want {
			t.Errorf("LogRetention(%q) = %v, want %v", c.tier, got, c.want)
		}
	}
}

// TestMetricRetention_MirrorsLogRetention confirms the two are equal
// by construction. If anyone adds a divergence later they'll need to
// explain why in docs/plans/retention-enforcement.md.
func TestMetricRetention_MirrorsLogRetention(t *testing.T) {
	for _, tier := range []string{"trial", "swatch", "yard", "bolt", "unknown"} {
		if MetricRetention(tier) != LogRetention(tier) {
			t.Errorf("MetricRetention(%q) diverges from LogRetention — see retention-enforcement.md", tier)
		}
	}
}
