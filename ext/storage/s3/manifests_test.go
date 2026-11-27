package s3

import "testing"

func TestParseS3URL(t *testing.T) {
	b, k, err := parseS3URL("s3://bkt/a/b/c.json")
	if err != nil || b != "bkt" || k != "a/b/c.json" {
		t.Fatalf("got %q %q %v", b, k, err)
	}
	for _, bad := range []string{"", "bkt/a", "s3://", "s3://bkt", "http://bkt/a"} {
		if _, _, err := parseS3URL(bad); err == nil {
			t.Errorf("parseS3URL(%q) succeeded, want error", bad)
		}
	}
}
