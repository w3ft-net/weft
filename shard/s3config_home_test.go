package shard

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

// TestConfigureDuckDB_EmptyHome reproduces the cold-shard Lambda failure:
// the Lambda execution environment has no HOME, so DuckDB cannot derive its
// extension directory and `INSTALL httpfs` aborts with
//
//	Can't find the home directory at ''
//
// before any network access. ConfigureDuckDB must set a writable
// home_directory so this never happens. The assertion is offline-safe: a
// missing-network environment yields a *download* error, not the
// home-directory error, so we only fail on the latter.
func TestConfigureDuckDB_EmptyHome(t *testing.T) {
	t.Setenv("HOME", "")

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer db.Close()

	cfg := &S3Config{Region: "us-east-1"}
	err = cfg.ConfigureDuckDB(db)
	if err != nil && strings.Contains(err.Error(), "home directory") {
		t.Fatalf("ConfigureDuckDB still fails on empty HOME: %v", err)
	}
}
