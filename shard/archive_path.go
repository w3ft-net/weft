package shard

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// layoutSegment is the last directory of an archive glob that names its layout.
var layoutSegment = regexp.MustCompile(`/([mk][1-9][0-9]*|sized)$`)

// SizedArchivePath is where a sized copy of the archive at old goes: beside it, as
// <old, without its layout directory or .parquet>.r<ns>/sized/**/*.parquet,
// versioned by now so a retry never collides with an earlier attempt's objects.
func SizedArchivePath(old string, now time.Time) string {
	base := layoutSegment.ReplaceAllString(strings.TrimSuffix(old, "/**/*.parquet"), "")
	base = strings.TrimSuffix(base, ".parquet")
	return fmt.Sprintf("%s.r%d/sized/**/*.parquet", base, now.UnixNano())
}
