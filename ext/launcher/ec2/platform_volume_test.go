package ec2

import "testing"

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
