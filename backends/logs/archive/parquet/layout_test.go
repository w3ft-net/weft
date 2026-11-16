package parquet

import (
	"testing"
	"time"
)

func TestPartitionMinutes_ReadsTheWidthFromTheArchiveGlob(t *testing.T) {
	cases := []struct {
		path string
		want int
		ok   bool
	}{
		{"s3://b/40-apps/shard-1.attempt-1/m180/**/*.parquet", 180, true},
		{"s3://b/40-apps/shard-1.attempt-12/m1/**/*.parquet", 1, true},
		{"/var/archive/shard-1.attempt-1/m594/**/*.parquet", 594, true},
		// Archives made before the width was recorded.
		{"s3://b/40-apps/shard-20261012T0000-93dc471d.attempt-1/**/*.parquet", 0, false},
		{"s3://b/40-apps/shard-1.attempt-1.parquet", 0, false},
		{"s3://b/40-apps/shard-1.r1793000000.parquet", 0, false},
		// A directory that merely starts with m is not a width.
		{"s3://b/m180x/**/*.parquet", 0, false},
		{"s3://b/m0/**/*.parquet", 0, false},
	}
	for _, c := range cases {
		got, ok := PartitionMinutes(c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("PartitionMinutes(%q) = %d, %v; want %d, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestChoosePartitionMinutes(t *testing.T) {
	const week = 7 * 24 * time.Hour
	cases := []struct {
		name  string
		bytes int64
		span  time.Duration
		want  int
	}{
		{"fits one file", 80 << 20, week, 0},
		{"just under the single-file limit", 99 << 20, week, 0},
		// 100 MiB is 6 files of 16 MiB: 1680-minute buckets over a week.
		{"just over it", 100 << 20, week, 1680},
		// 2.67 GB of parquet is 159 files (the count floors): 64-minute buckets.
		{"a shard that used to get one-day files", 2_673_000_000, week, 64},
		// 2.9 GB is 172 files: 59-minute buckets.
		{"a shard that used to get hourly files", 2_900_000_000, week, 59},
		{"never narrower than a minute", 50 << 30, 10 * time.Minute, 1},
		{"no time span", 2 << 30, 0, 0},
	}
	for _, c := range cases {
		if got := ChoosePartitionMinutes(c.bytes, c.span); got != c.want {
			t.Errorf("%s: ChoosePartitionMinutes(%d, %s) = %d, want %d", c.name, c.bytes, c.span, got, c.want)
		}
	}
}

func TestSizedPartitionPath(t *testing.T) {
	cases := []struct {
		path         string
		minutes      int
		keyed, sized bool
	}{
		{"s3://b/40-apps/shard-1.attempt-1/sized/**/*.parquet", 0, true, true},
		{"/var/archive/shard-1.attempt-3.r17/sized/**/*.parquet", 0, true, true},
		// the first keyed layout, one width for every key: still read, no longer written
		{"s3://b/40-apps/shard-1.attempt-1/k1440/**/*.parquet", 1440, true, false},
		{"s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet", 1440, false, false},
		{"s3://b/40-apps/shard-1.attempt-1/**/*.parquet", 0, false, false},
		{"s3://b/40-apps/shard-1.attempt-1.parquet", 0, false, false},
		// a directory that merely starts with these is not the layout
		{"s3://b/sizedx/**/*.parquet", 0, false, false},
		{"s3://b/k0/**/*.parquet", 0, false, false},
	}
	for _, c := range cases {
		minutes, _ := PartitionMinutes(c.path)
		if minutes != c.minutes || IsKeyed(c.path) != c.keyed || IsSized(c.path) != c.sized {
			t.Errorf("%q: width %d keyed %v sized %v; want width %d keyed %v sized %v",
				c.path, minutes, IsKeyed(c.path), IsSized(c.path), c.minutes, c.keyed, c.sized)
		}
	}
}

func TestPickWidth(t *testing.T) {
	const mib = 1 << 20
	day := 24 * time.Hour
	for _, c := range []struct {
		name string
		est  int64
		span time.Duration
		want int
	}{
		// 1 GiB is 64 target files; a week over 64 is 157 minutes, so the next width up
		{"a week of 1 GiB", 1024 * mib, 7 * day, 360},
		// 200 MiB is 12.5 files; 3 days over 12.5 is 5.8 hours, so 6 hours
		{"three days of 200 MiB", 200 * mib, 3 * day, 360},
		// less than one file: one bucket for the whole span, the widest
		{"a key too small for a second file", 10 * mib, 7 * day, 10080},
		{"nothing to size", 0, 7 * day, 10080},
		// more files than minutes: the narrowest
		{"an hour of 1 GiB", 1024 * mib, time.Hour, 60},
		{"a day of 100 GiB", 100 * 1024 * mib, day, 60},
		// all rows at one instant: one bucket
		{"no span", 1024 * mib, 0, 10080},
		// 192 MiB is exactly 12 files; 3 days over 12 is exactly 360 minutes, which stays 360
		{"on a width exactly", 192 * mib, 3 * day, 360},
	} {
		got := PickWidth(c.est, c.span)
		if got != c.want {
			t.Errorf("%s: PickWidth = %d, want %d", c.name, got, c.want)
		}
	}
}

// Every width PickWidth can return is one a reader's bucket arithmetic and a
// day-aligned grid both handle: whole hours, dividing a week or a day.
func TestKeyedWidthsAreAligned(t *testing.T) {
	for _, w := range KeyedWidths {
		if w%60 != 0 {
			t.Errorf("width %d is not a whole number of hours", w)
		}
		if 10080%w != 0 && w%1440 != 0 {
			t.Errorf("width %d neither divides a week nor is whole days", w)
		}
	}
}
