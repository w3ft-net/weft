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
		"medium":  defaultShardVolumeGB, // present but 0 → fallback
		"unknown": defaultShardVolumeGB, // absent → fallback
	}
	for tier, want := range cases {
		if got := d.volumeSizeGB(tier); got != want {
			t.Errorf("volumeSizeGB(%q) = %d, want %d", tier, got, want)
		}
	}

	// Nil map (the dev/test ShardDeps) must still resolve to the default.
	var nilDeps ShardDeps
	if got := nilDeps.volumeSizeGB("small"); got != defaultShardVolumeGB {
		t.Errorf("nil-map volumeSizeGB = %d, want %d", got, defaultShardVolumeGB)
	}
}

// TestParseShardVolumeSizes_ParsesAndSkipsJunk asserts a positive integer is
// parsed per tier and that blank/malformed/non-positive values are skipped
// (left to the launch-time default), never poisoning the map with a 0.
func TestParseShardVolumeSizes_ParsesAndSkipsJunk(t *testing.T) {
	t.Setenv("SHARD_SMALL_VOLUME_GB", "25")
	t.Setenv("SHARD_MEDIUM_VOLUME_GB", "not-a-number")
	t.Setenv("SHARD_LARGE_VOLUME_GB", "0")

	got := parseShardVolumeSizes()
	if got["small"] != 25 {
		t.Errorf("small: want 25, got %d", got["small"])
	}
	if _, ok := got["medium"]; ok {
		t.Errorf("medium: malformed value should be skipped, got %d", got["medium"])
	}
	if _, ok := got["large"]; ok {
		t.Errorf("large: non-positive value should be skipped, got %d", got["large"])
	}
}
