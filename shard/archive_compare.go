package shard

import (
	"database/sql"
	"fmt"
)

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
