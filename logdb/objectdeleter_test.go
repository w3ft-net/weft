package logdb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFileDeleter(t *testing.T) {
	dir := t.TempDir()

	// Single file.
	f := filepath.Join(dir, "s.parquet")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := (LocalFileDeleter{}).DeleteParquet(f); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Errorf("file not removed: %v", err)
	}
	// Idempotent: deleting a gone file is success.
	if err := (LocalFileDeleter{}).DeleteParquet(f); err != nil {
		t.Errorf("second delete should be no-op: %v", err)
	}

	// Partitioned: a "<dir>/**/*.parquet" glob removes the whole dir.
	part := filepath.Join(dir, "shard")
	os.MkdirAll(filepath.Join(part, "_part=1"), 0755)
	os.WriteFile(filepath.Join(part, "_part=1", "data.parquet"), []byte("x"), 0644)
	if err := (LocalFileDeleter{}).DeleteParquet(part + "/**/*.parquet"); err != nil {
		t.Fatalf("delete glob: %v", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Errorf("partition dir not removed: %v", err)
	}
}

func TestParseS3Path(t *testing.T) {
	b, k, err := parseS3Path("s3://weft-cold/weft/shard-1.parquet")
	if err != nil || b != "weft-cold" || k != "weft/shard-1.parquet" {
		t.Fatalf("got (%q, %q, %v)", b, k, err)
	}
	for _, bad := range []string{"weft-cold/x", "s3://", "s3://bucket-only"} {
		if _, _, err := parseS3Path(bad); err == nil {
			t.Errorf("parseS3Path(%q) should error", bad)
		}
	}
}
