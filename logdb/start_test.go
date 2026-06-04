package logdb

import (
	"reflect"
	"testing"
)

// TestParseShardSubnets covers the SHARD_<TIER>_SUBNETS env parsing that
// selects EC2-direct launching: az=subnet pairs per tier, empty when unset,
// malformed pairs skipped (not fatal).
func TestParseShardSubnets(t *testing.T) {
	t.Run("unset → empty", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_SUBNETS", "")
		t.Setenv("SHARD_MEDIUM_SUBNETS", "")
		t.Setenv("SHARD_LARGE_SUBNETS", "")
		if got := parseShardSubnets(); len(got) != 0 {
			t.Fatalf("want empty, got %v", got)
		}
	})

	t.Run("multi-tier multi-az", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_SUBNETS", "us-east-2a=subnet-0aa, us-east-2b=subnet-0bb")
		t.Setenv("SHARD_MEDIUM_SUBNETS", "us-east-2a=subnet-0cc")
		t.Setenv("SHARD_LARGE_SUBNETS", "")
		got := parseShardSubnets()
		want := map[string]map[string]string{
			"small":  {"us-east-2a": "subnet-0aa", "us-east-2b": "subnet-0bb"},
			"medium": {"us-east-2a": "subnet-0cc"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseShardSubnets:\n got  %v\n want %v", got, want)
		}
	})

	t.Run("malformed pairs skipped", func(t *testing.T) {
		t.Setenv("SHARD_SMALL_SUBNETS", "us-east-2a=subnet-ok,garbage,=noaz,nosubnet=")
		t.Setenv("SHARD_MEDIUM_SUBNETS", "")
		t.Setenv("SHARD_LARGE_SUBNETS", "")
		got := parseShardSubnets()
		want := map[string]map[string]string{"small": {"us-east-2a": "subnet-ok"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parseShardSubnets:\n got  %v\n want %v", got, want)
		}
	})
}

// TestParseShardTemplates covers SHARD_<TIER>_LAUNCH_TEMPLATE parsing:
// one launch-template id per tier, only present tiers included.
func TestParseShardTemplates(t *testing.T) {
	t.Setenv("SHARD_SMALL_LAUNCH_TEMPLATE", " lt-0small ")
	t.Setenv("SHARD_MEDIUM_LAUNCH_TEMPLATE", "")
	t.Setenv("SHARD_LARGE_LAUNCH_TEMPLATE", "lt-0large")
	got := parseShardTemplates()
	want := map[string]string{"small": "lt-0small", "large": "lt-0large"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseShardTemplates:\n got  %v\n want %v", got, want)
	}
}
