package shard

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/graybeardcorp/weft/shape"
)

// WriteArchive writes the rows query returns through db to dstPath as an
// archive: a sized archive (see IsSized) or, for any other path, one Parquet
// file. The keyed layout is only ever read, so it is refused. With s3cfg nil
// and dstPath not an s3:// URL, the directory it lands in is created.
func WriteArchive(db *sql.DB, query, dstPath string, layout shape.Layout, s3cfg *S3Config) error {
	if IsKeyed(dstPath) && !IsSized(dstPath) {
		return fmt.Errorf("cannot write the %s layout: only sized archives are written", dstPath)
	}
	if s3cfg == nil && !strings.HasPrefix(dstPath, "s3://") {
		parent := filepath.Dir(dstPath)
		if IsSized(dstPath) {
			parent = strings.TrimSuffix(dstPath, "/**/*.parquet")
		}
		if err := os.MkdirAll(parent, 0755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", dstPath, err)
		}
	}
	if IsSized(dstPath) {
		if err := copySized(db, query, layout, strings.TrimSuffix(dstPath, "/**/*.parquet")); err != nil {
			return fmt.Errorf("copy to %s: %w", dstPath, err)
		}
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET)", query, dstPath)); err != nil {
		return fmt.Errorf("copy to %s: %w", dstPath, err)
	}
	return nil
}
