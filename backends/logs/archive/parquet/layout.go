package parquet

// The Parquet archive layouts: how an archive is split into files, written into
// its path so it travels wherever the path does, and the arithmetic that picks
// the split. None of it depends on the engine that writes or reads the files.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An archive too big for one file is split into time buckets of a whole number
// of minutes, counted from the Unix epoch. The width is picked per shard so the
// files come out near TargetFileBytes (ChoosePartitionMinutes), and is written
// into the archive path as a segment under the attempt directory:
//
//	<prefix>/<shard>.attempt-N/m<minutes>/_part=<bucket start>/data_0.parquet
//
// so it travels wherever the path does (shard_records.parquet_path, the shard
// descriptor, the Lambda request) and a reader can name the exact partitions a
// time range falls in. It sits below the attempt directory so the attempt's
// delete globs (ExportOutputPaths) still cover it. Archives made before the
// width was recorded have no such segment.
//
// A sized archive also splits by log key, and each key chooses the width of its
// own time buckets (PickWidth), so a big key and a small key both come out in
// files near TargetFileBytes:
//
//	<prefix>/<shard>.attempt-N/sized/customer_id=C/namespace=N/service=S/kind=K/_w=<minutes>/_part=<bucket start>/data_0.parquet
//
// so a query that names a key opens only that key's directories, and a time
// bound reaches only its buckets (partBound). The reader has to know the layout,
// because it types the key columns (ParquetReadExpr) and bounds time by _w. The
// first keyed layout, k<minutes> in place of m<minutes> with no _w, gave every
// key one width; it is still read, and no longer written.

const (
	// TargetFileBytes is the file size the width is picked for. An archived
	// read of a few minutes opens only the files holding them, so smaller
	// files make it cheaper: measured on the two archived shards (fresh DuckDB,
	// a 5-minute slice), 500 MB files took 2.3 s, 220 MB files 1.3-1.5 s, and
	// 11-14 MB files 0.7-1.0 s. A whole-day read goes the other way (1.6 s,
	// 2.1 s, 2.8 s respectively), by a smaller margin.
	TargetFileBytes = 16 * 1024 * 1024 // 16 MiB

	// ParquetToDBRatio is the archive's Parquet size over the store's own data size, as measured on DuckDB:
	// measured on the first two archived shards (3.0 GiB -> 2.7 GiB and 2.77 GiB
	// -> 2.49 GiB). The estimate used to be 0.4, which put files at twice the
	// target.
	ParquetToDBRatio = 0.9
)

// KeyedWidths are the time buckets, in minutes, a sized archive gives a key:
// whole hours, each dividing a week or being whole days, so buckets line up
// with the calendar. A reader needs no list of them: it reads the width of
// each key from the _w directory.
var KeyedWidths = []int{60, 120, 360, 720, 1440, 2880, 10080}

// PickWidth is the bucket width for one key whose rows come to estParquetBytes
// over span, so that its files come out near TargetFileBytes: the narrowest
// width in KeyedWidths that is not narrower than span over the files wanted. A
// key that fills less than one file, or has no span, gets the widest.
func PickWidth(estParquetBytes int64, span time.Duration) int {
	widest := KeyedWidths[len(KeyedWidths)-1]
	files := float64(estParquetBytes) / TargetFileBytes
	if files <= 1 || span <= 0 {
		return widest
	}
	want := span.Minutes() / files
	for _, w := range KeyedWidths {
		if float64(w) >= want {
			return w
		}
	}
	return widest
}

var partitionSegment = regexp.MustCompile(`/([mk])([1-9][0-9]*)/\*\*/\*\.parquet$`)

// sizedSegment is the directory of a sized archive: split by log key, and by a
// time bucket whose width each key has chosen for itself (the _w directory).
var sizedSegment = regexp.MustCompile(`/sized/\*\*/\*\.parquet$`)

// PartitionMinutes reads the bucket width, in minutes, from an archive's glob
// path. ok is false for an archive that doesn't record it.
func PartitionMinutes(parquetPath string) (minutes int, ok bool) {
	m := partitionSegment.FindStringSubmatch(parquetPath)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[2])
	return n, err == nil
}

// IsKeyed reports whether an archive's glob path names a layout split by log
// key: the sized one, or the first keyed one (k<minutes>, a single width for
// every key), which is still read but no longer written.
func IsKeyed(parquetPath string) bool {
	return IsSized(parquetPath) || isKeyedWidth(parquetPath)
}

// isKeyedWidth reports whether the path is the first keyed layout, k<minutes>.
func isKeyedWidth(parquetPath string) bool {
	m := partitionSegment.FindStringSubmatch(parquetPath)
	return m != nil && m[1] == "k"
}

// IsSized reports whether an archive's glob path names the sized layout, where
// each key's time buckets have a width of their own.
func IsSized(parquetPath string) bool { return sizedSegment.MatchString(parquetPath) }

// BucketStart is the start of the minutes-wide bucket holding t: the value the
// export writes as _part for a row at t.
func BucketStart(t time.Time, minutes int) time.Time {
	// Not time.Truncate, which counts from the year 1, not the epoch.
	width := int64(minutes) * 60
	return time.Unix(t.Unix()/width*width, 0).UTC()
}

// ChoosePartitionMinutes picks the bucket width that gives files near
// TargetFileBytes for estParquetBytes of data spread over span. 0 means a
// single file.
func ChoosePartitionMinutes(estParquetBytes int64, span time.Duration) int {
	if estParquetBytes < 100*1024*1024 || span <= 0 {
		return 0
	}
	files := estParquetBytes / TargetFileBytes
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

// ExportOutputSuffix names one attempt's output: the export writes
// <prefix>/<shard-id>.attempt-N[.parquet], never into the location a failed
// attempt used. DuckDB's OVERWRITE_OR_IGNORE only replaces same-named files, so
// a retry that shared a directory with a partial earlier attempt would leave the
// old files in place and the archive glob would read both (rows double-count).
func ExportOutputSuffix(attempt int) string {
	return fmt.Sprintf(".attempt-%d", attempt)
}

// ExportOutputPaths lists the delete targets for one attempt's output: the
// hive-partitioned directory form and the single-file form, in the shapes
// logdb.ObjectDeleter takes. They are exact (the directory ends in a slash
// before its glob, the file in .parquet), so attempt 1 can never match
// attempt 10.
func ExportOutputPaths(prefix, shardID string, attempt int) []string {
	base := strings.TrimRight(prefix, "/") + "/" + shardID + ExportOutputSuffix(attempt)
	return []string{base + "/**/*.parquet", base + ".parquet"}
}

// ManifestKey is where a shard's export manifest lives under the archive
// prefix. The coordinator derives the same key to poll for completion.
func ManifestKey(prefix, shardID string) string {
	return strings.TrimRight(prefix, "/") + "/_manifests/" + shardID + ".json"
}
