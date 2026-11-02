package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shard"
)

// convert-archive turns a downloaded archive into a sized copy and checks the
// copy against the original and against the export manifest. The download and the
// upload around it are the script's (scripts/convert-archives.py).

// downloaded writes a small archive under root as the script's download would
// leave it (root mirrors the bucket) and returns a manifest that describes it.
// layout is "m" for a time-partitioned archive or "single" for one file.
func downloaded(t *testing.T, root, layout string) shard.ExportManifest {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE logs AS
		SELECT 'line ' || i AS raw, TIMESTAMP '2026-10-14 00:00:00' + ((i * 3600) || ' seconds')::INTERVAL AS ts,
		       CASE WHEN i % 5 = 0 THEN 2 ELSE 1 END AS customer_id, 'weft' AS namespace,
		       CASE WHEN i % 2 = 0 THEN 'api' ELSE 'web' END AS service, 'main' AS kind
		FROM range(60) r(i)`); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "40-apps", "shard-1.attempt-1")
	var path string
	switch layout {
	case "m":
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf(`COPY (SELECT *, time_bucket(INTERVAL '1440 minutes', ts, TIMESTAMP '1970-01-01') AS _part FROM logs)
			TO '%s/m1440' (FORMAT PARQUET, PARTITION_BY (_part))`, base)); err != nil {
			t.Fatal(err)
		}
		path = "s3://bkt/40-apps/shard-1.attempt-1/m1440/**/*.parquet"
	case "k":
		// the first keyed layout, as the deployed export wrote it
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf(`COPY (SELECT *, time_bucket(INTERVAL '54 minutes', ts, TIMESTAMP '1970-01-01') AS _part FROM logs)
			TO '%s/k54' (FORMAT PARQUET, PARTITION_BY (customer_id, namespace, service, kind, _part))`, base)); err != nil {
			t.Fatal(err)
		}
		path = "s3://bkt/40-apps/shard-1.attempt-1/k54/**/*.parquet"
	case "single":
		if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf("COPY logs TO '%s.parquet' (FORMAT PARQUET)", base)); err != nil {
			t.Fatal(err)
		}
		path = "s3://bkt/40-apps/shard-1.attempt-1.parquet"
	default:
		t.Fatalf("layout %q", layout)
	}
	return shard.ExportManifest{
		ShardID: "shard-1", ParquetPath: path, Rows: 60,
		Customers: []shard.CustomerStat{{CustomerID: 1, RowCount: 48}, {CustomerID: 2, RowCount: 12}},
	}
}

func TestConvertArchive_ConvertsEachLayout(t *testing.T) {
	for _, layout := range []string{"m", "k", "single"} {
		t.Run(layout, func(t *testing.T) {
			root, out := t.TempDir(), t.TempDir()
			m := downloaded(t, root, layout)
			now := time.Unix(1_800_000_000, 0)
			res, err := convertArchive(m, root, out, now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != "converted" || res.Rows != 60 || res.Keys != 4 || res.Files == 0 || res.Bytes == 0 {
				t.Errorf("result = %+v", res)
			}
			if res.OldPath != m.ParquetPath || res.NewPath != shard.SizedArchivePath(m.ParquetPath, now) {
				t.Errorf("paths: old %q new %q", res.OldPath, res.NewPath)
			}
			// the copy is where the upload expects it: out mirrors the bucket
			local := filepath.Join(out, strings.TrimPrefix(res.NewPath, "s3://bkt/"))
			if keys, err := shard.CompareArchives(shard.LocalPathOf(m.ParquetPath, root), local, nil); err != nil || keys != 4 {
				t.Errorf("the copy at %s: %d keys, err %v", local, keys, err)
			}
		})
	}
}

// A download that is missing a file does not match what the export recorded: it
// must stop before converting something incomplete.
func TestConvertArchive_RefusesAnIncompleteDownload(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	m := downloaded(t, root, "m")
	files, _ := filepath.Glob(filepath.Join(root, "40-apps", "shard-1.attempt-1", "m1440", "*", "*.parquet"))
	if len(files) < 2 {
		t.Fatalf("fixture has %d files", len(files))
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	_, err := convertArchive(m, root, out, time.Now())
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("an incomplete download was converted, or failed oddly: %v", err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("wrote %d entries for an archive it refused", len(entries))
	}
}

func TestConvertArchive_LeavesASizedArchiveAlone(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	m := shard.ExportManifest{ShardID: "s", ParquetPath: "s3://bkt/40-apps/shard-1.attempt-1.r9/sized/**/*.parquet"}
	res, err := convertArchive(m, root, out, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "skipped" || res.NewPath != "" {
		t.Errorf("result = %+v, want skipped", res)
	}
}
