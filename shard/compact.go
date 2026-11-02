package shard

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// CaptureStats reads an archived parquet and returns its per-customer row
// counts and ts spans — the same shape Archive emits, but computed from a
// file rather than a live handle. The janitor uses it to backfill
// shard_archive_stats for a shard archived before stats existed (or by an
// un-rolled old shard binary), so a later pass can classify it without a
// re-scan. Pass s3cfg for s3:// paths, nil for local.
func CaptureStats(parquetPath string, s3cfg *S3Config) ([]CustomerStat, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			return nil, err
		}
	}
	return scanCustomerStats(context.Background(), db, fmt.Sprintf(
		"SELECT COALESCE(customer_id, 0), MIN(ts), MAX(ts), COUNT(*) FROM %s GROUP BY 1",
		ParquetReadExpr(parquetPath)))
}

// CustomerCutoff is one customer's deletion boundary for a retention
// rewrite: rows for CustomerID with ts < Cutoff are dropped. A customer
// NOT represented in the cutoff set is preserved in full — that is how
// the janitor protects an unmapped customer (no active subscription)
// from deletion. See docs/plans/retention-janitor.md.
type CustomerCutoff struct {
	CustomerID int64     `json:"customer_id"`
	Cutoff     time.Time `json:"cutoff"`
}

// CompactResult reports the outcome of RewriteRetention. JSON-tagged: it
// doubles as the compactor Lambda's response body.
type CompactResult struct {
	RowsKept    int64 `json:"rows_kept"`
	RowsDeleted int64 `json:"rows_deleted"`
	// OutputPath is where the survivors were written, or "" when no rows
	// survived — in which case nothing was written and the caller should
	// delete the source object outright (and retire the shard).
	OutputPath string `json:"output_path,omitempty"`
	// Stats is the per-customer breakdown of the survivors, so the caller
	// can refresh shard_archive_stats from the rewritten file without a
	// re-scan. Empty when nothing survived.
	Stats []CustomerStat `json:"stats,omitempty"`
}

// RewriteRetention reads the Parquet at srcPath, drops each cutoff
// customer's rows older than its cutoff, and writes the survivors to a
// single recompacted Parquet at dstPath (fresh row-group statistics fall
// out of the COPY). Customers absent from cutoffs are kept in full.
//
// Pass s3cfg to read/write S3 (both paths are s3:// URLs); pass nil for
// local-filesystem paths (the dev parquet layout), in which case dstPath's
// parent directory is created. The source may be a single file or a
// hive-partitioned glob.
//
// When no rows survive, the result's OutputPath is "" and nothing is
// written; the caller deletes the source and moves the shard to GONE.
func RewriteRetention(srcPath, dstPath string, cutoffs []CustomerCutoff, s3cfg *S3Config) (CompactResult, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return CompactResult{}, fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			return CompactResult{}, err
		}
	}

	if _, err := db.Exec("CREATE TEMP TABLE cutoffs (customer_id BIGINT, cutoff TIMESTAMP)"); err != nil {
		return CompactResult{}, fmt.Errorf("create cutoffs: %w", err)
	}
	for _, c := range cutoffs {
		if _, err := db.Exec("INSERT INTO cutoffs VALUES (?, ?)", c.CustomerID, c.Cutoff); err != nil {
			return CompactResult{}, fmt.Errorf("insert cutoff %d: %w", c.CustomerID, err)
		}
	}

	src := ParquetReadExpr(srcPath)
	// Keep a row when its customer has no cutoff (LEFT JOIN miss → preserve)
	// or its ts is still within retention. A NULL customer never matches a
	// cutoff, so it is preserved too.
	keep := fmt.Sprintf(
		"SELECT l.* FROM %s l LEFT JOIN cutoffs c ON l.customer_id = c.customer_id "+
			"WHERE c.customer_id IS NULL OR l.ts >= c.cutoff", src)

	var total, kept int64
	if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", src)).Scan(&total); err != nil {
		return CompactResult{}, fmt.Errorf("count source: %w", err)
	}
	if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM (%s)", keep)).Scan(&kept); err != nil {
		return CompactResult{}, fmt.Errorf("count survivors: %w", err)
	}

	res := CompactResult{RowsKept: kept, RowsDeleted: total - kept}
	if kept == 0 {
		return res, nil // nothing survives → caller deletes the source
	}

	stats, err := scanCustomerStats(context.Background(), db, fmt.Sprintf(
		"SELECT COALESCE(customer_id, 0), MIN(ts), MAX(ts), COUNT(*) FROM (%s) GROUP BY 1", keep))
	if err != nil {
		return CompactResult{}, fmt.Errorf("survivor stats: %w", err)
	}
	res.Stats = stats

	// A keyed destination (see IsKeyed) is written keyed, whatever the source
	// was: a keyed one, a time-only one, or a single file. dstPath is then the
	// glob, and the COPY goes to its directory. _part is rebuilt from ts for the
	// destination's bucket width, replacing the source's own if it has one, so
	// converting a time-only archive of another width, or a single file with
	// no _part, comes out the same.
	copyTo, partitionBy, out := dstPath, "", keep
	if IsKeyed(dstPath) {
		copyTo = strings.TrimSuffix(dstPath, "/**/*.parquet")
		partitionBy = ", PARTITION_BY (" + keyedPartitionBy + "), OVERWRITE_OR_IGNORE"
		minutes, _ := PartitionMinutes(dstPath)
		var hasPart int
		if err := db.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM (DESCRIBE SELECT * FROM (%s) k) WHERE column_name = '_part'", keep)).Scan(&hasPart); err != nil {
			return CompactResult{}, fmt.Errorf("describe source: %w", err)
		}
		cols := "k.*"
		if hasPart > 0 {
			cols = "k.* EXCLUDE (_part)"
		}
		// Sorted for the reason exportParquet gives; this can spill to disk.
		out = fmt.Sprintf("SELECT %s, %s AS _part FROM (%s) k ORDER BY %s, ts", cols, partitionExpr(minutes, "k.ts"), keep, keyedPartitionBy)
	}
	if s3cfg == nil && !strings.HasPrefix(dstPath, "s3://") {
		parent := filepath.Dir(dstPath)
		if partitionBy != "" {
			parent = copyTo
		}
		if err := os.MkdirAll(parent, 0755); err != nil {
			return CompactResult{}, fmt.Errorf("mkdir for %s: %w", dstPath, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET%s)", out, copyTo, partitionBy)); err != nil {
		return CompactResult{}, fmt.Errorf("copy survivors to %s: %w", dstPath, err)
	}
	res.OutputPath = dstPath
	return res, nil
}
