package shard

import (
	"fmt"
	"strings"
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

// A downloaded archive mirrors the bucket under a root directory.
func TestLocalPathOf(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"s3://bkt/40-apps/s.attempt-1/m60/**/*.parquet", "/r/40-apps/s.attempt-1/m60/**/*.parquet"},
		{"s3://bkt/40-apps/s.parquet", "/r/40-apps/s.parquet"},
	} {
		if got := LocalPathOf(c.in, "/r/"); got != c.want {
			t.Errorf("LocalPathOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// VerifyAgainstManifest checks an archive against what its export recorded: the
// total rows, and the rows of each customer.
func TestVerifyAgainstManifest(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6) // customers 1 (5 keys) and 2 (1 key), 6 rows a key
	_, glob, _ := exportTo(t, ds, 1440)
	good := ExportManifest{
		ShardID: "s", Rows: 36,
		Customers: []CustomerStat{{CustomerID: 1, RowCount: 30}, {CustomerID: 2, RowCount: 6}},
	}
	if err := VerifyAgainstManifest(glob, good, nil); err != nil {
		t.Fatalf("an archive that matches its manifest failed: %v", err)
	}

	short := good
	short.Rows = 37
	if err := VerifyAgainstManifest(glob, short, nil); err == nil || !strings.Contains(err.Error(), "37") {
		t.Errorf("a manifest claiming one more row passed or failed oddly: %v", err)
	}

	swapped := good
	swapped.Customers = []CustomerStat{{CustomerID: 1, RowCount: 29}, {CustomerID: 2, RowCount: 7}}
	if err := VerifyAgainstManifest(glob, swapped, nil); err == nil {
		t.Error("a manifest with the right total but the wrong customers passed")
	}

	missing := good
	missing.Customers = []CustomerStat{{CustomerID: 1, RowCount: 30}}
	missing.Rows = 30
	if err := VerifyAgainstManifest(glob, missing, nil); err == nil {
		t.Error("an archive holding a customer the manifest does not list passed")
	}
}
