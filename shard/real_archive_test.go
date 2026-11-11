package shard

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shape"
)

// A dry run of the conversion on a real archive, run by hand before the janitor
// does it for real. Skipped unless WEFT_REAL_ARCHIVE is set to a directory
// holding a downloaded archive (the contents of an s3://.../<shard>.attempt-N/
// prefix, whatever its layout). Set AWS_LAMBDA_FUNCTION_MEMORY_SIZE to the
// compactor Lambda's memory to run under its limits. It converts exactly as the
// compactor does, then checks the copy against the original, key by key.
//
//	WEFT_REAL_ARCHIVE=/path/to/attempt-1 AWS_LAMBDA_FUNCTION_MEMORY_SIZE=2048 \
//	  go test -tags test -run TestRealArchive_Conversion -v ./shard/
func TestRealArchive_Conversion(t *testing.T) {
	root := os.Getenv("WEFT_REAL_ARCHIVE")
	if root == "" {
		t.Skip("set WEFT_REAL_ARCHIVE to a downloaded archive directory to run")
	}
	src := root + "/**/*.parquet"
	dstDir := filepath.Join(t.TempDir(), "shard.attempt-1.r1")
	dst := dstDir + "/sized/**/*.parquet"

	// How much the sort spills: the peak size of the spill directory.
	spill := filepath.Join(os.TempDir(), "duckdb-spill")
	var peak int64
	var mu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
				if n := dirBytes(spill); n > 0 {
					mu.Lock()
					peak = max(peak, n)
					mu.Unlock()
				}
			}
		}
	}()

	start := time.Now()
	copied, err := CopyArchive(src, dst, shape.Legacy(), nil)
	took := time.Since(start)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("conversion failed after %s: %v", took.Round(time.Second), err)
	}
	// Peak resident memory of the whole test process, which is DuckDB's plus Go's.
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	rss := ru.Maxrss // bytes on macOS, KiB on Linux
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	t.Logf("converted %d rows in %s; spill peaked at %d MiB; process peak %d MiB; AWS_LAMBDA_FUNCTION_MEMORY_SIZE=%q",
		copied, took.Round(time.Second), peak>>20, rss>>20, os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE"))

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Every key holds the same rows in the copy as in the original.
	keys, err := CompareArchives(src, dst, nil)
	if err != nil {
		t.Errorf("the copy differs from the original: %v", err)
	}
	t.Logf("%d keys compared", keys)

	// What the copy looks like: widths per key, and the file counts and sizes.
	rows, err := db.Query(`SELECT service, kind, _w, count(DISTINCT _part), count(*) FROM ` + ParquetReadExpr(dst, shape.Legacy()) +
		` GROUP BY ALL ORDER BY count(*) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var svc, kind string
		var w, buckets, n int64
		if err := rows.Scan(&svc, &kind, &w, &buckets, &n); err != nil {
			t.Fatal(err)
		}
		t.Logf("  %-14s %-10s %6d rows  %5d-minute buckets x %d", svc, kind, n, w, buckets)
	}
	var sizes []int64
	filepath.WalkDir(dstDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".parquet") {
			if fi, e := d.Info(); e == nil {
				sizes = append(sizes, fi.Size())
			}
		}
		return nil
	})
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	var total int64
	for _, s := range sizes {
		total += s
	}
	if len(sizes) > 0 {
		t.Logf("%d files, %d MiB; median %s, largest %s, smallest %s", len(sizes), total>>20,
			kib(sizes[len(sizes)/2]), kib(sizes[len(sizes)-1]), kib(sizes[0]))
	}
	t.Logf("original: %d MiB", dirBytes(root)>>20)
}

func kib(n int64) string {
	if n >= 1<<20 {
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<10), 'f', 0, 64) + " KiB"
}

// dirBytes is the total size of the files under dir.
func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
