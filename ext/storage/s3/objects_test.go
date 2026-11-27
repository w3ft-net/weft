package s3

import "testing"

func TestParseS3Path(t *testing.T) {
	b, k, err := parseS3Path("s3://weft-cold/weft/shard-1.parquet")
	if err != nil || b != "weft-cold" || k != "weft/shard-1.parquet" {
		t.Fatalf("got (%q, %q, %v)", b, k, err)
	}
	for _, bad := range []string{"weft-cold/x", "s3://", "s3://bucket-only"} {
		if _, _, err := parseS3Path(bad); err == nil {
			t.Errorf("parseS3Path(%q) should error", bad)
		}
	}
}
