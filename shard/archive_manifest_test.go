package shard

import (
	"fmt"
	"testing"
	"time"
)

// SizedArchivePath is where a sized copy of an archive goes: beside the original,
// versioned by the instant so a retry never collides with an earlier attempt.
func TestSizedArchivePath(t *testing.T) {
	now := time.Unix(1_700_000_000, 5)
	tag := fmt.Sprintf(".r%d", now.UnixNano())
	for _, c := range []struct{ old, want string }{
		// time-only, whatever the width, or none recorded
		{"s3://b/40-apps/shard-1.attempt-1/m60/**/*.parquet", "s3://b/40-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet"},
		{"s3://b/40-apps/shard-20261012.attempt-1/**/*.parquet", "s3://b/40-apps/shard-20261012.attempt-1" + tag + "/sized/**/*.parquet"},
		// a single file, and one a retention pass already rewrote
		{"s3://b/40-apps/shard-1.parquet", "s3://b/40-apps/shard-1" + tag + "/sized/**/*.parquet"},
		{"s3://b/40-apps/shard-1.r1700.parquet", "s3://b/40-apps/shard-1.r1700" + tag + "/sized/**/*.parquet"},
		// the first keyed layout, and a sized archive being re-sized
		{"s3://b/39-apps/shard-1.attempt-1/k54/**/*.parquet", "s3://b/39-apps/shard-1.attempt-1" + tag + "/sized/**/*.parquet"},
		{"s3://b/39-apps/shard-1.attempt-1.r17/sized/**/*.parquet", "s3://b/39-apps/shard-1.attempt-1.r17" + tag + "/sized/**/*.parquet"},
	} {
		got := SizedArchivePath(c.old, now)
		if got != c.want {
			t.Errorf("SizedArchivePath(%q) = %q, want %q", c.old, got, c.want)
		}
		if !IsSized(got) {
			t.Errorf("%q is not recognised as a sized archive", got)
		}
	}
}
