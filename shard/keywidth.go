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

	"github.com/graybeardcorp/weft/shape"
)

// parquetPerRawByte estimates the Parquet bytes a row takes per byte of its text
// plus rowOverheadBytes, for a writer that does not know the size of what it is
// rewriting (the compactor). Measured on a real 18.7M-row archive: 2,370 MiB of
// Parquet for 3,455 MiB of text, 0.69 (the Parquet also holds the payload and
// fields columns). A synthetic trial gave 0.29, which made the widths 2.3 times
// too wide on the real archive, with files of up to 82 MiB against the 16 MiB target.
const parquetPerRawByte = 0.7

// rowOverheadBytes is what a row costs beyond its text, for the same estimate.
const rowOverheadBytes = 64

// sizedSelect reads src as a sized archive's rows: every column of src, the
// key's own bucket width as _w, and _part, the start of the row's bucket of
// that width. It joins the key_widths table that createKeyWidths made on the
// connection it will run on. The join treats a missing customer, namespace,
// service or kind as a value, since rows from before they were filled in have
// none. The rows are not sorted: see partitionedWriteSettings.
func sizedSelect(src string, layout shape.Layout) string {
	var on strings.Builder
	for i, c := range layout.Columns {
		fmt.Fprintf(&on, "l.%s IS NOT DISTINCT FROM k.p%d AND ", c.Name, i)
	}
	return fmt.Sprintf(`SELECT l.*, k.w AS _w,
		time_bucket(to_minutes(k.w), l.ts, TIMESTAMP '1970-01-01') AS _part
		FROM %s l JOIN key_widths k
		ON %sl.namespace IS NOT DISTINCT FROM k.kn
		AND l.service IS NOT DISTINCT FROM k.ks AND l.kind IS NOT DISTINCT FROM k.kk`, src, on.String())
}

// partitionBy is the partition columns of a sized archive, in directory order:
// the layout's, then the key and the time bucket.
func partitionBy(layout shape.Layout) string {
	var b strings.Builder
	for _, c := range layout.Columns {
		b.WriteString(c.Name + ", ")
	}
	return b.String() + "namespace, service, kind, _w, _part"
}

// Open files a partitioned write may keep: one per partition being written.
// DuckDB's default is 100, and past it closes a partition's file to open
// another's and starts a new file each time it comes back: 1,905 files for 308
// partitions in a 6M-row trial. Raised, the same write made 308 files, with no
// sort and so no spill. A Lambda allows 1,024 open files in all, so there the cap
// leaves room for the rest; past it the write still completes, in more files.
const (
	partitionedWriteOpenFiles       = 20000
	partitionedWriteOpenFilesLambda = 900
)

// partitionedWriteSettings are the DuckDB settings for the sized write.
func partitionedWriteSettings() []string {
	n := partitionedWriteOpenFiles
	if inLambda() {
		n = partitionedWriteOpenFilesLambda
	}
	return []string{fmt.Sprintf("SET partitioned_write_max_open_files=%d", n)}
}

// createKeyWidths makes the key_widths temp table on conn: for each key in src,
// the bucket width its files should have. A key's share of estParquetBytes is its
// share of the rows' text, and its span is its own first row to its last. With
// estParquetBytes 0 the total is estimated from the text (parquetPerRawByte).
func createKeyWidths(ctx context.Context, conn *sql.Conn, src string, layout shape.Layout, estParquetBytes int64) error {
	var cols, ddl, marks string
	for i, c := range layout.Columns {
		cols += c.Name + ", "
		ddl += fmt.Sprintf("p%d %s, ", i, c.Type.SQL())
		marks += "?, "
	}
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT %snamespace, service, kind,
		sum(length(raw) + %d), min(ts), max(ts) FROM %s GROUP BY ALL`, cols, rowOverheadBytes, src))
	if err != nil {
		return fmt.Errorf("size keys: %w", err)
	}
	// key is one log key's partition values, then its namespace, service and
	// kind; a NULL stays nil.
	type keyStat struct {
		key    []any
		bytes  int64
		lo, hi time.Time
	}
	var keys []keyStat
	var total int64
	for rows.Next() {
		s := keyStat{key: make([]any, len(layout.Columns)+3)}
		dest := make([]any, 0, len(s.key)+3)
		for i := range s.key {
			dest = append(dest, &s.key[i])
		}
		dest = append(dest, &s.bytes, &s.lo, &s.hi)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return fmt.Errorf("size keys: %w", err)
		}
		keys = append(keys, s)
		total += s.bytes
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("size keys: %w", err)
	}
	rows.Close()
	if estParquetBytes <= 0 {
		estParquetBytes = int64(float64(total) * parquetPerRawByte)
	}

	if _, err := conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE key_widths
		(`+ddl+`kn TEXT, ks TEXT, kk TEXT, w INTEGER)`); err != nil {
		return fmt.Errorf("create key_widths: %w", err)
	}
	for _, s := range keys {
		est := int64(float64(estParquetBytes) * float64(s.bytes) / float64(total))
		args := append(s.key, pickWidth(est, s.hi.Sub(s.lo)))
		if _, err := conn.ExecContext(ctx, `INSERT INTO key_widths VALUES (`+marks+`?, ?, ?, ?)`, args...); err != nil {
			return fmt.Errorf("record key width: %w", err)
		}
	}
	return nil
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
