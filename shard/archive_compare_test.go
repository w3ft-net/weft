package shard

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// CompareArchives is the check that stands between a converted archive and the
// pointer swap: it must pass a faithful copy and fail one that lost anything.

func TestCompareArchives_PassesAFaithfulCopy(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6)
	_, src, _ := exportTo(t, ds, 1440)
	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/sized/**/*.parquet"
	if _, err := RewriteRetention(src, dst, nil, nil); err != nil {
		t.Fatal(err)
	}
	keys, err := CompareArchives(src, dst, nil)
	if err != nil {
		t.Fatalf("a faithful copy failed the comparison: %v", err)
	}
	if keys != len(keyedFixture) {
		t.Errorf("compared %d keys, want %d", keys, len(keyedFixture))
	}
}

func TestCompareArchives_FailsACopyThatLostRows(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6)
	_, src, _ := exportTo(t, ds, 1440)
	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/sized/**/*.parquet"
	if _, err := RewriteRetention(src, dst, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Lose one key's file: the copy is short by that key's rows.
	files, _ := filepath.Glob(filepath.Join(dstDir, "sized", "customer_id=2", "*", "*", "*", "*", "*", "data_0.parquet"))
	if len(files) == 0 {
		t.Fatal("no file to remove")
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	_, err := CompareArchives(src, dst, nil)
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("a copy missing a key's rows passed or failed oddly: %v", err)
	}
}

// A copy with every row but a changed line is a different archive.
func TestCompareArchives_FailsACopyWithAChangedLine(t *testing.T) {
	ds := legacyShardWith(t, keyedFixture, 6)
	_, src, _ := exportTo(t, ds, 1440)
	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/sized/**/*.parquet"
	if _, err := RewriteRetention(src, dst, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Rewrite one file's lines in place, keeping its row count.
	files, _ := filepath.Glob(filepath.Join(dstDir, "sized", "customer_id=2", "*", "*", "*", "*", "*", "data_0.parquet"))
	if len(files) == 0 {
		t.Fatal("no file to alter")
	}
	altered := files[0] + ".new"
	db := openMem(t)
	if _, err := db.Exec("COPY (SELECT * REPLACE (raw || '!' AS raw) FROM read_parquet('" + files[0] + "')) TO '" + altered + "' (FORMAT PARQUET)"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(altered, files[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := CompareArchives(src, dst, nil); err == nil {
		t.Fatal("a copy with a changed line passed the comparison")
	}
}
