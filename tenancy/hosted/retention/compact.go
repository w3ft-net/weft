package retention

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// CaptureStats reads an archived parquet and returns its per-customer row
// counts and ts spans — the same shape Archive emits, but computed from a
// file rather than a live handle. The janitor uses it to backfill
// shard_archive_stats for a shard archived before stats existed (or by an
// un-rolled old shard binary), so a later pass can classify it without a
// re-scan. Pass s3cfg for s3:// paths, nil for local.
func CaptureStats(parquetPath string, layout shape.Layout, s3cfg *shard.S3Config) ([]shard.PartitionStat, error) {
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
	return shard.PartitionStats(context.Background(), db, layout, shard.ParquetReadExpr(parquetPath, layout))
}

// CustomerCutoff is one customer's deletion boundary for a retention
// rewrite: rows for CustomerID with ts < Cutoff are dropped. A customer
// NOT represented in the cutoff set is preserved in full — that is how
// the janitor protects an unmapped customer (no active subscription)
// from deletion. See docs/plans/retention-janitor.md.
type CustomerCutoff struct {
	CustomerID int64 `json:"customer_id"`
	// PartitionValues name the partition the cutoff applies to, as text in
	// layout order. Absent from an older caller, which names a customer: the
	// legacy layout reads CustomerID instead.
	PartitionValues []string  `json:"partition_values,omitempty"`
	Cutoff          time.Time `json:"cutoff"`
}

// values are the partition values the cutoff names under layout.
func (c CustomerCutoff) values(layout shape.Layout) ([]string, error) {
	switch {
	case c.PartitionValues != nil:
		return c.PartitionValues, nil
	case layout.IsLegacy():
		return []string{strconv.FormatInt(c.CustomerID, 10)}, nil
	case len(layout.Columns) == 0:
		return nil, nil // the one partition
	}
	return nil, fmt.Errorf("a cutoff must name partition values under layout %s, which has no customer_id", layout.Fingerprint)
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
	Stats []shard.PartitionStat `json:"stats,omitempty"`
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
func RewriteRetention(srcPath, dstPath string, cutoffs []CustomerCutoff, layout shape.Layout, s3cfg *shard.S3Config) (CompactResult, error) {
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

	src := shard.ParquetReadExpr(srcPath, layout)
	// One cutoffs row per partition, its columns typed as the layout's. hit is
	// always true, so a LEFT JOIN miss is detectable in a layout with no
	// partition columns too.
	var ddl, on, marks string
	for i, col := range layout.Columns {
		ddl += fmt.Sprintf("p%d %s, ", i, col.Type.SQL())
		on += fmt.Sprintf("l.%s = c.p%d AND ", col.Name, i)
		marks += "?, "
	}
	if _, err := db.Exec("CREATE TEMP TABLE cutoffs (" + ddl + "hit BOOLEAN, cutoff TIMESTAMP)"); err != nil {
		return CompactResult{}, fmt.Errorf("create cutoffs: %w", err)
	}
	for _, c := range cutoffs {
		vals, err := c.values(layout)
		if err != nil {
			return CompactResult{}, err
		}
		args, err := layout.Parse(vals)
		if err != nil {
			return CompactResult{}, fmt.Errorf("cutoff %v: %w", vals, err)
		}
		args = append(args, true, c.Cutoff)
		if _, err := db.Exec("INSERT INTO cutoffs VALUES ("+marks+"?, ?)", args...); err != nil {
			return CompactResult{}, fmt.Errorf("insert cutoff %v: %w", vals, err)
		}
	}
	// Keep a row when its partition has no cutoff (LEFT JOIN miss → preserve)
	// or its ts is still within retention. A NULL partition value never matches
	// a cutoff, so those rows are preserved too.
	keep := fmt.Sprintf(
		"SELECT l.* FROM %s l LEFT JOIN cutoffs c ON %strue WHERE c.hit IS NULL OR l.ts >= c.cutoff", src, on)

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

	stats, err := shard.PartitionStats(context.Background(), db, layout, "("+keep+")")
	if err != nil {
		return CompactResult{}, fmt.Errorf("survivor stats: %w", err)
	}
	res.Stats = stats

	if err := shard.WriteArchive(db, keep, dstPath, layout, s3cfg); err != nil {
		return CompactResult{}, err
	}
	res.OutputPath = dstPath
	return res, nil
}
