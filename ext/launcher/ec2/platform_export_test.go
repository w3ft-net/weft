package ec2

import "testing"

func TestParseExportConfig(t *testing.T) {
	tiers := map[string]map[string]string{
		"small":  {"us-east-2a": "subnet-small-a", "us-east-2b": "subnet-small-b"},
		"medium": {"us-east-2a": "subnet-medium-a", "us-east-2c": "subnet-medium-c"},
	}

	t.Run("unset means disabled", func(t *testing.T) {
		t.Setenv("EXPORT_LAUNCH_TEMPLATE", "")
		t.Setenv("EXPORT_MAX_CONCURRENT", "")
		c := parseExportConfig(tiers)
		if c.LaunchTemplate != "" {
			t.Fatalf("launch template = %q, want empty", c.LaunchTemplate)
		}
		if ok, why := c.usable("s3://weft-cold/warp"); ok || why == "" {
			t.Fatalf("usable = %v %q, want false with a reason", ok, why)
		}
	})

	t.Run("subnets merge across tiers with the smaller tier winning", func(t *testing.T) {
		t.Setenv("EXPORT_LAUNCH_TEMPLATE", "lt-export")
		c := parseExportConfig(tiers)
		want := map[string]string{"us-east-2a": "subnet-small-a", "us-east-2b": "subnet-small-b", "us-east-2c": "subnet-medium-c"}
		if len(c.Subnets) != len(want) {
			t.Fatalf("subnets = %v, want %v", c.Subnets, want)
		}
		for az, sn := range want {
			if c.Subnets[az] != sn {
				t.Errorf("subnet[%s] = %q, want %q", az, c.Subnets[az], sn)
			}
		}
	})

	t.Run("max concurrent", func(t *testing.T) {
		t.Setenv("EXPORT_LAUNCH_TEMPLATE", "lt-export")
		for env, want := range map[string]int{"": 0, "3": 3, "0": 0, "-1": 0, "abc": 0} {
			t.Setenv("EXPORT_MAX_CONCURRENT", env)
			if got := parseExportConfig(tiers).MaxConcurrent; got != want {
				t.Errorf("EXPORT_MAX_CONCURRENT=%q → %d, want %d (0 means the default)", env, got, want)
			}
		}
	})

	t.Run("usable needs an s3 archive prefix and subnets", func(t *testing.T) {
		t.Setenv("EXPORT_LAUNCH_TEMPLATE", "lt-export")
		c := parseExportConfig(tiers)
		if ok, _ := c.usable("s3://weft-cold/warp"); !ok {
			t.Fatal("not usable with a template, subnets and an s3 prefix")
		}
		// The export writes its manifest to the archive prefix over S3 and the
		// coordinator reads it back; a local directory can't connect the two.
		if ok, why := c.usable("/var/data/parquet"); ok || why == "" {
			t.Fatalf("usable with a local parquet dir: %v %q", ok, why)
		}
		if ok, _ := (exportConfig{LaunchTemplate: "lt-export"}).usable("s3://b/p"); ok {
			t.Fatal("usable with no subnets")
		}
	})
}
