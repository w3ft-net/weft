package shard

import (
	"database/sql"
	"fmt"

	"github.com/w3ft-net/weft/shape"
)

// copyArchive rewrites the archive at src to dst whole and returns how many
// rows it copied: the sized writer reading a parquet, as the compactor's rewrite
// does. Nothing is written for an archive with no rows.
func copyArchive(src, dst string, layout shape.Layout) (int64, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return 0, fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()
	read := ParquetReadExpr(src, layout)
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + read).Scan(&n); err != nil {
		return 0, fmt.Errorf("count source: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	return n, WriteArchive(db, "SELECT * FROM "+read, dst, layout, nil)
}
