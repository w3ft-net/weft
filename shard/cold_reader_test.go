package shard

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
)

// The Lambda reader runs the query where the endpoint is, so an archive with no
// endpoint cannot be read through it; with one it opens without calling it.
func TestLambdaReader_NeedsAnEndpoint(t *testing.T) {
	r := LambdaReader(nil, "us-east-2")
	if _, err := r.Open(archive.Archive{ID: "s1", Path: "s3://b/a.parquet"}); err == nil || !strings.Contains(err.Error(), "function URL") {
		t.Fatalf("open with no endpoint: %v", err)
	}
	h, err := r.Open(archive.Archive{ID: "s1", Path: "s3://b/a.parquet", Endpoint: "https://example.invalid/fn"})
	if err != nil || h == nil {
		t.Fatalf("open: %v, %v", h, err)
	}
	h.Close()
}

func TestColdReaders_ReadEveryParquetFormatInDuckDBsDialect(t *testing.T) {
	want := map[string]bool{}
	for _, f := range parquet.Formats() {
		want[f.ID()] = true
	}
	for name, r := range map[string]archive.Reader{"lambda": LambdaReader(nil, ""), "local": LocalReader(nil)} {
		got := r.Reads()
		if len(got) != len(want) {
			t.Errorf("%s reads %v, want %d formats", name, got, len(want))
		}
		for _, id := range got {
			if !want[id] {
				t.Errorf("%s reads %q, which is not a parquet format", name, id)
			}
		}
		if r.Dialect().Name() != "duckdb" {
			t.Errorf("%s dialect = %s", name, r.Dialect().Name())
		}
	}
}
