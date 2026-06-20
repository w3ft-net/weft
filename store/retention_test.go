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
		tier Tier
		want time.Duration
	}{
		{TierTrial, 24 * time.Hour},
		{TierSwatch, 30 * 24 * time.Hour},
		{TierYard, 90 * 24 * time.Hour},
		{TierBolt, 180 * 24 * time.Hour},
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
	for _, tier := range []Tier{TierTrial, TierSwatch, TierYard, TierBolt, "unknown"} {
		if MetricRetention(tier) != LogRetention(tier) {
			t.Errorf("MetricRetention(%q) diverges from LogRetention — see retention-enforcement.md", tier)
		}
	}
}

// TestVolumeAllowance pins the monthly ingestion quotas. These are
// contractual — if they change, the Stripe graduated-tier free-quota
// boundary on each overage price (docs/plans/stripe.md) and the
// pricing-page advertised numbers must change in lockstep.
func TestVolumeAllowance(t *testing.T) {
	cases := []struct {
		tier Tier
		want int64
	}{
		{TierTrial, 1_000_000_000},
		{TierSwatch, 10_000_000_000},
		{TierYard, 100_000_000_000},
		{TierBolt, 500_000_000_000},
		{"", 1_000_000_000},
	}
	for _, c := range cases {
		if got := VolumeAllowance(c.tier); got != c.want {
			t.Errorf("VolumeAllowance(%q) = %d, want %d", c.tier, got, c.want)
		}
	}
}

// TestOverageRatePerGB pins the per-GB overage cents. Like
// VolumeAllowance these match the Stripe price config; the helper
// is a projection for the dashboard, not the billing source of
// truth.
func TestOverageRatePerGB(t *testing.T) {
	cases := []struct {
		tier Tier
		want int
	}{
		{TierTrial, 0},
		{TierSwatch, 200},
		{TierYard, 100},
		{TierBolt, 75},
		{"", 0},
	}
	for _, c := range cases {
		if got := OverageRatePerGB(c.tier); got != c.want {
			t.Errorf("OverageRatePerGB(%q) = %d, want %d", c.tier, got, c.want)
		}
	}
}
