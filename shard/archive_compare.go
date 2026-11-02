package shard

import (
	"database/sql"
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

// VerifyAgainstManifest checks an archive against what its export recorded: the
// total rows, and the rows of each customer (a row with no customer counts under
// 0, as the export's stats do). An archive missing rows, holding extra ones, or
// holding a customer the manifest does not list fails. Pass s3cfg for an s3://
// path, nil for a local one.
func VerifyAgainstManifest(path string, m ExportManifest, s3cfg *S3Config) error {
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
	rows, err := db.Query("SELECT COALESCE(customer_id, 0), count(*) FROM " + ParquetReadExpr(path) + " GROUP BY 1")
	if err != nil {
		return fmt.Errorf("count %s: %w", path, err)
	}
	defer rows.Close()
	got := map[int64]int64{}
	var total int64
	for rows.Next() {
		var c, n int64
		if err := rows.Scan(&c, &n); err != nil {
			return err
		}
		got[c], total = n, total+n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if total != m.Rows {
		return fmt.Errorf("%s holds %d rows, the manifest of %s says %d", path, total, m.ShardID, m.Rows)
	}
	want := map[int64]int64{}
	for _, c := range m.Customers {
		want[c.CustomerID] = c.RowCount
	}
	for c, n := range want {
		if got[c] != n {
			return fmt.Errorf("%s holds %d rows of customer %d, the manifest of %s says %d", path, got[c], c, m.ShardID, n)
		}
	}
	for c, n := range got {
		if _, listed := want[c]; !listed {
			return fmt.Errorf("%s holds %d rows of customer %d, which the manifest of %s does not list", path, n, c, m.ShardID)
		}
	}
	return nil
}

// CompareArchives checks that the archive at dst holds the rows of the one at
// src, which is the check between converting an archive and swapping it in. For
// each log key (customer, namespace, service, kind) the two must agree on the
// number of rows, a sum of a hash of every line, and the first and last time. A
// key present in one and not the other, a missing row, an added row and a changed
// line each make them differ. It returns how many keys src has. Both are archive
// paths as ParquetReadExpr takes them; pass s3cfg for s3:// paths, nil for local.
func CompareArchives(src, dst string, s3cfg *S3Config) (keys int, err error) {
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

	fingerprint := func(from string) string {
		return `SELECT customer_id, namespace, service, kind, count(*) AS n,
			sum(CAST(hash(raw) AS HUGEINT)) AS h, min(ts) AS lo, max(ts) AS hi FROM ` + from + ` GROUP BY ALL`
	}
	a, b := fingerprint(ParquetReadExpr(src)), fingerprint(ParquetReadExpr(dst))

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
