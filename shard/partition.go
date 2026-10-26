package shard

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// An archive too big for one file is split into time buckets of a whole number
// of minutes, counted from the Unix epoch. The width is picked per shard so the
// files come out near targetFileBytes (choosePartitionMinutes), and is written
// into the archive path as a segment under the attempt directory:
//
//	<prefix>/<shard>.attempt-N/m<minutes>/_part=<bucket start>/data_0.parquet
//
// so it travels wherever the path does (shard_records.parquet_path, the shard
// descriptor, the Lambda request) and a reader can name the exact partitions a
// time range falls in. It sits below the attempt directory so the attempt's
// delete globs (ExportOutputPaths) still cover it. Archives made before the
// width was recorded have no such segment.

const (
	// targetFileBytes is the file size the width is picked for. An archived
	// read of a few minutes opens only the files holding them, so smaller
	// files make it cheaper: measured on the two archived shards (fresh DuckDB,
	// a 5-minute slice), 500 MB files took 2.3 s, 220 MB files 1.3-1.5 s, and
	// 11-14 MB files 0.7-1.0 s. A whole-day read goes the other way (1.6 s,
	// 2.1 s, 2.8 s respectively), by a smaller margin.
	targetFileBytes = 16 * 1024 * 1024 // 16 MiB

	// parquetToDBRatio is the archive's Parquet size over the DuckDB file's,
	// measured on the first two archived shards (3.0 GiB -> 2.7 GiB and 2.77 GiB
	// -> 2.49 GiB). The estimate used to be 0.4, which put files at twice the
	// target.
	parquetToDBRatio = 0.9
)

var partitionSegment = regexp.MustCompile(`/m([1-9][0-9]*)/\*\*/\*\.parquet$`)

// PartitionMinutes reads the bucket width, in minutes, from an archive's glob
// path. ok is false for an archive that doesn't record it.
func PartitionMinutes(parquetPath string) (minutes int, ok bool) {
	m := partitionSegment.FindStringSubmatch(parquetPath)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// partitionExpr is the _part expression for minutes-wide buckets of the
// timestamp expression ts. The epoch origin makes a bucket's start computable
// from the width alone (BucketStart).
func partitionExpr(minutes int, ts string) string {
	return fmt.Sprintf("time_bucket(INTERVAL '%d minutes', %s, TIMESTAMP '1970-01-01')", minutes, ts)
}

// BucketStart is the start of the minutes-wide bucket holding t: the value the
// export writes as _part for a row at t.
func BucketStart(t time.Time, minutes int) time.Time {
	// Not time.Truncate, which counts from the year 1, not the epoch.
	width := int64(minutes) * 60
	return time.Unix(t.Unix()/width*width, 0).UTC()
}

// choosePartitionMinutes picks the bucket width that gives files near
// targetFileBytes for estParquetBytes of data spread over span. 0 means a
// single file.
func choosePartitionMinutes(estParquetBytes int64, span time.Duration) int {
	if estParquetBytes < 100*1024*1024 || span <= 0 {
		return 0
	}
	files := estParquetBytes / targetFileBytes
	if files < 2 {
		return 0
	}
	width := span / time.Duration(files)
	minutes := int((width + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	return minutes
}
