package shard

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/shape"
)

// CaptureStats reads an archived parquet and returns its per-customer row
// counts and ts spans — the same shape Archive emits, but computed from a
// file rather than a live handle. The janitor uses it to backfill
// shard_archive_stats for a shard archived before stats existed (or by an
// un-rolled old shard binary), so a later pass can classify it without a
// re-scan. Pass s3cfg for s3:// paths, nil for local.
func CaptureStats(parquetPath string, layout shape.Layout, s3cfg *S3Config) ([]CustomerStat, error) {
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
	return partitionStats(context.Background(), db, layout, ParquetReadExpr(parquetPath, layout))
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
func RewriteRetention(srcPath, dstPath string, cutoffs []CustomerCutoff, layout shape.Layout, s3cfg *S3Config) (CompactResult, error) {
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

	src := ParquetReadExpr(srcPath, layout)
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

	stats, err := partitionStats(context.Background(), db, layout, "("+keep+")")
	if err != nil {
		return CompactResult{}, fmt.Errorf("survivor stats: %w", err)
	}
	res.Stats = stats

	if IsKeyed(dstPath) && !IsSized(dstPath) {
		return CompactResult{}, fmt.Errorf("cannot write the %s layout: only sized archives are written", dstPath)
	}
	if s3cfg == nil && !strings.HasPrefix(dstPath, "s3://") {
		parent := filepath.Dir(dstPath)
		if IsSized(dstPath) {
			parent = strings.TrimSuffix(dstPath, "/**/*.parquet")
		}
		if err := os.MkdirAll(parent, 0755); err != nil {
			return CompactResult{}, fmt.Errorf("mkdir for %s: %w", dstPath, err)
		}
	}
	if IsSized(dstPath) {
		if err := copySized(db, keep, layout, strings.TrimSuffix(dstPath, "/**/*.parquet")); err != nil {
			return CompactResult{}, fmt.Errorf("copy survivors to %s: %w", dstPath, err)
		}
	} else if _, err := db.Exec(fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET)", keep, dstPath)); err != nil {
		return CompactResult{}, fmt.Errorf("copy survivors to %s: %w", dstPath, err)
	}
	res.OutputPath = dstPath
	return res, nil
}

// lambdaThreads caps DuckDB's threads in a Lambda: the compactor's function has
// about two cores, and each thread needs its own buffers inside the memory limit.
const lambdaThreads = 2

// lambdaMemoryMB is the memory of the Lambda function this runs in, in MB, or 0
// when it is not in one.
func lambdaMemoryMB() int {
	mb, err := strconv.Atoi(os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE"))
	if err != nil || mb <= 0 {
		return 0
	}
	return mb
}

func inLambda() bool { return lambdaMemoryMB() > 0 }

// lambdaSettings keep DuckDB inside the function it runs in. In a Lambda DuckDB
// sees the host's memory, not the function's, so the limit is 60% of the
// function's (AWS_LAMBDA_FUNCTION_MEMORY_SIZE), leaving room for what DuckDB does
// not count, shared among at most lambdaThreads threads; elsewhere the defaults
// stand. Anything that does spill goes to the temp directory, because DuckDB's
// default is relative to a working directory a Lambda cannot write (a larger
// than memory sort failed there with "Failed to create directory .tmp:
// Permission denied").
func lambdaSettings() []string {
	var stmts []string
	if mb := lambdaMemoryMB(); mb > 0 {
		stmts = append(stmts,
			fmt.Sprintf("SET memory_limit='%dMB'", mb*60/100),
			fmt.Sprintf("SET threads=%d", min(runtime.NumCPU(), lambdaThreads)))
	}
	return append(stmts, "SET temp_directory='"+filepath.Join(os.TempDir(), "duckdb-spill")+"'")
}

// copySized writes the rows of query to dir as a sized archive (see IsSized),
// whatever layout they came from: a sized or keyed archive, a time-only one, or
// a single file. Their own _part and _w are dropped and rebuilt from ts and the
// widths chosen for the keys in query, so a time-only archive of another width,
// or a single file with no _part, comes out the same. The rows are not sorted:
// a whole archive does not fit in a Lambda's disk to sort (a 2.5 GiB real archive
// needed more than 6.5 GiB of spill); partitionedWriteSettings keeps it from
// splitting partitions into many files instead.
func copySized(db *sql.DB, query string, layout shape.Layout, dir string) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, stmt := range append(lambdaSettings(), partitionedWriteSettings()...) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}

	src := "(" + query + ")"
	var drop []string
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(
		"SELECT column_name FROM (DESCRIBE SELECT * FROM (%s)) WHERE column_name IN ('_part', '_w')", query))
	if err != nil {
		return fmt.Errorf("describe source: %w", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		drop = append(drop, c)
	}
	rows.Close()
	if len(drop) > 0 {
		src = fmt.Sprintf("(SELECT * EXCLUDE (%s) FROM (%s))", strings.Join(drop, ", "), query)
	}
	if err := createKeyWidths(ctx, conn, src, layout, 0); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, fmt.Sprintf(
		"COPY (%s) TO '%s' (FORMAT PARQUET, PARTITION_BY (%s), OVERWRITE_OR_IGNORE)", sizedSelect(src, layout), dir, partitionBy(layout)))
	return err
}
