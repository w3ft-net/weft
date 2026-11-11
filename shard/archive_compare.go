package shard

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shape"
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

// LocalPathOf maps an s3://bucket/key archive path to the same key under root,
// where a download that mirrors the bucket puts it. A glob keeps its glob.
func LocalPathOf(s3Path, root string) string {
	rest := strings.TrimPrefix(s3Path, "s3://")
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[i+1:]
	}
	return strings.TrimRight(root, "/") + "/" + rest
}

// partitionKey is a comparable form of one group's partition values.
func partitionKey(values []string) string { return strings.Join(values, "\x1f") }

// partitionLabel is a group's partition values as a person reads them.
func partitionLabel(key string) string { return strings.ReplaceAll(key, "\x1f", "/") }

// VerifyAgainstManifest and CompareArchives are the operator's checks on an
// archive written in layout.
//
// VerifyAgainstManifest checks an archive against what its export recorded: the
// total rows, and the rows of each partition (a row with no value in a partition
// column counts as 0, or ” for text, as the export's stats do). An archive
// missing rows, holding extra ones, or holding a partition the manifest does not
// list fails. Pass s3cfg for an s3:// path, nil for a local one.
func VerifyAgainstManifest(path string, m ExportManifest, layout shape.Layout, s3cfg *S3Config) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			return err
		}
	}
	cols, group := partitionColumns(layout)
	query := "SELECT " + strings.Join(append(cols, ""), ", ") + "count(*) FROM " + ParquetReadExpr(path, layout)
	if len(group) > 0 {
		query += " GROUP BY " + strings.Join(group, ", ")
	}
	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("count %s: %w", path, err)
	}
	defer rows.Close()
	got := map[string]int64{}
	var total int64
	for rows.Next() {
		values := make([]string, len(cols))
		dest := make([]any, 0, len(cols)+1)
		for i := range values {
			dest = append(dest, &values[i])
		}
		var n int64
		if err := rows.Scan(append(dest, &n)...); err != nil {
			return err
		}
		got[partitionKey(values)], total = n, total+n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total != m.Rows {
		return fmt.Errorf("%s holds %d rows, the manifest of %s says %d", path, total, m.ShardID, m.Rows)
	}
	want := map[string]int64{}
	for _, c := range m.Partitions {
		want[partitionKey(c.PartitionValues)] = c.RowCount
	}
	for k, n := range want {
		if got[k] != n {
			return fmt.Errorf("%s holds %d rows of partition %q, the manifest of %s says %d", path, got[k], partitionLabel(k), m.ShardID, n)
		}
	}
	for k, n := range got {
		if _, listed := want[k]; !listed {
			return fmt.Errorf("%s holds %d rows of partition %q, which the manifest of %s does not list", path, n, partitionLabel(k), m.ShardID)
		}
	}
	return nil
}

// CompareArchives checks that the archive at dst holds the rows of the one at
// src, which is the check between converting an archive and swapping it in. For
// each log key (partition values, namespace, service, kind) the two must agree
// on the number of rows, a sum of a hash of every line, and the first and last
// time. A key present in one and not the other, a missing row, an added row and
// a changed line each make them differ. It returns how many keys src has. Both
// are archive paths as ParquetReadExpr takes them, written in layout; pass s3cfg
// for s3:// paths, nil for local.
func CompareArchives(src, dst string, layout shape.Layout, s3cfg *S3Config) (keys int, err error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return 0, fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			return 0, err
		}
	}

	var names []string
	for _, c := range layout.Columns {
		names = append(names, c.Name)
	}
	fingerprint := func(from string) string {
		return "SELECT " + strings.Join(append(names, "namespace, service, kind"), ", ") + `, count(*) AS n,
			sum(CAST(hash(raw) AS HUGEINT)) AS h, min(ts) AS lo, max(ts) AS hi FROM ` + from + ` GROUP BY ALL`
	}
	a, b := fingerprint(ParquetReadExpr(src, layout)), fingerprint(ParquetReadExpr(dst, layout))

	var differ int
	if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM ((%[1]s EXCEPT %[2]s) UNION ALL (%[2]s EXCEPT %[1]s))", a, b)).Scan(&differ); err != nil {
		return 0, fmt.Errorf("compare %s with %s: %w", src, dst, err)
	}
	if differ != 0 {
		return 0, fmt.Errorf("%d keys differ between %s and %s", differ, src, dst)
	}
	if err := db.QueryRow("SELECT count(*) FROM (" + a + ")").Scan(&keys); err != nil {
		return 0, err
	}
	return keys, nil
}
