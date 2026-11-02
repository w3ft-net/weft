package shard

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// parquetPerRawByte estimates the Parquet bytes a row's text takes, for a
// writer that does not know the size of what it is rewriting (the compactor).
// It was 0.29 on a synthetic trial, so it errs towards narrower buckets than
// needed, not wider.
const parquetPerRawByte = 0.3

// rowOverheadBytes is what a row costs beyond its text, for the same estimate.
const rowOverheadBytes = 64

// sizedSelect reads src as a sized archive's rows: every column of src, the
// key's own bucket width as _w, and _part, the start of the row's bucket of
// that width. It joins the key_widths table that createKeyWidths made on the
// connection it will run on. The join treats a missing customer, namespace,
// service or kind as a value, since rows from before they were filled in have
// none. The rows are not sorted: see partitionedWriteSettings.
func sizedSelect(src string) string {
	return fmt.Sprintf(`SELECT l.*, k.w AS _w,
		time_bucket(to_minutes(k.w), l.ts, TIMESTAMP '1970-01-01') AS _part
		FROM %s l JOIN key_widths k
		ON l.customer_id IS NOT DISTINCT FROM k.kc AND l.namespace IS NOT DISTINCT FROM k.kn
		AND l.service IS NOT DISTINCT FROM k.ks AND l.kind IS NOT DISTINCT FROM k.kk`, src)
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
func createKeyWidths(ctx context.Context, conn *sql.Conn, src string, estParquetBytes int64) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT customer_id, namespace, service, kind,
		sum(length(raw) + %d), min(ts), max(ts) FROM %s GROUP BY ALL`, rowOverheadBytes, src))
	if err != nil {
		return fmt.Errorf("size keys: %w", err)
	}
	type keyStat struct {
		customer          sql.NullInt64
		namespace, svc, k sql.NullString
		bytes             int64
		lo, hi            time.Time
	}
	var keys []keyStat
	var total int64
	for rows.Next() {
		var s keyStat
		if err := rows.Scan(&s.customer, &s.namespace, &s.svc, &s.k, &s.bytes, &s.lo, &s.hi); err != nil {
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
		(kc BIGINT, kn TEXT, ks TEXT, kk TEXT, w INTEGER)`); err != nil {
		return fmt.Errorf("create key_widths: %w", err)
	}
	for _, s := range keys {
		est := int64(float64(estParquetBytes) * float64(s.bytes) / float64(total))
		if _, err := conn.ExecContext(ctx, `INSERT INTO key_widths VALUES (?, ?, ?, ?, ?)`,
			s.customer, s.namespace, s.svc, s.k, pickWidth(est, s.hi.Sub(s.lo))); err != nil {
			return fmt.Errorf("record key width: %w", err)
		}
	}
	return nil
}
