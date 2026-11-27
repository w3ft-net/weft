package logdb

import "testing"

// TestVolumeSizeGB_FallsBackPerTier asserts the per-tier lookup honors an
// override when present and falls back to the default otherwise — including the
// nil-map (dev/test) case — and never returns 0, which CreateForShard rejects.
func TestVolumeSizeGB_FallsBackPerTier(t *testing.T) {
	d := &ShardDeps{VolumeSizesGB: map[string]int32{"small": 25, "large": 100, "medium": 0}}
	cases := map[string]int32{
		"small":   25,                   // explicit override
		"large":   100,                  // explicit override
		"medium":  DefaultShardVolumeGB, // present but 0 → fallback
		"unknown": DefaultShardVolumeGB, // absent → fallback
	}
	for tier, want := range cases {
		if got := d.volumeSizeGB(tier); got != want {
			t.Errorf("volumeSizeGB(%q) = %d, want %d", tier, got, want)
		}
	}

	// Nil map (the dev/test ShardDeps) must still resolve to the default.
	var nilDeps ShardDeps
	if got := nilDeps.volumeSizeGB("small"); got != DefaultShardVolumeGB {
		t.Errorf("nil-map volumeSizeGB = %d, want %d", got, DefaultShardVolumeGB)
	}
}
