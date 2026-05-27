package logdb

import (
	"reflect"
	"testing"
)

// TestParseShardASGs covers the SHARD_<TIER>_ASGS env parsing that
// selects per-AZ ASGProvisioner launching: az=asg pairs per tier, empty
// when unset, malformed pairs skipped (not fatal).
func TestParseShardASGs(t *testing.T) {
	t.Run("unset → empty", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_ASGS", "")
		t.Setenv("SHARD_MEDIUM_ASGS", "")
		t.Setenv("SHARD_LARGE_ASGS", "")
		if got := parseShardASGs(); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("multi-tier multi-az", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_ASGS", "us-east-2a=weft-shard-small-2a, us-east-2b=weft-shard-small-2b")
		t.Setenv("SHARD_MEDIUM_ASGS", "us-east-2a=weft-shard-medium-2a")
		t.Setenv("SHARD_LARGE_ASGS", "")
		got := parseShardASGs()
		want := map[string]map[string]string{
			"small":  {"us-east-2a": "weft-shard-small-2a", "us-east-2b": "weft-shard-small-2b"},
			"medium": {"us-east-2a": "weft-shard-medium-2a"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseShardASGs:\n got  %v\n want %v", got, want)
		}
	})

	t.Run("malformed pairs skipped", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_ASGS", "us-east-2a=good,garbage,=noaz,noasg=")
		t.Setenv("SHARD_MEDIUM_ASGS", "")
		t.Setenv("SHARD_LARGE_ASGS", "")
		got := parseShardASGs()
		want := map[string]map[string]string{"small": {"us-east-2a": "good"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseShardASGs:\n got  %v\n want %v", got, want)
		}
	})
}
