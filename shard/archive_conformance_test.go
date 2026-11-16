package shard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive/archivetest"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// writeFrom archives rows with write, which is handed an empty shard holding
// them and a fresh attempt directory.
func writeFrom(t *testing.T, rows []*pb.LogRow, write func(ds *DuckDBShard, dir string) (string, error)) string {
	t.Helper()
	ds, err := NewDuckDBShard("conformance", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "conformance.attempt-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := write(ds, dir)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The archives the open build writes, read back by the DuckDB reader.
func TestDuckDB_WritesArchivesItsReaderReads(t *testing.T) {
	ctx := context.Background()
	archivetest.Run(t, archivetest.Spec{
		Formats: parquet.Formats(),
		Cases: []archivetest.Case{
			{Format: parquet.FormatFile, Write: func(t *testing.T, rows []*pb.LogRow) string {
				return writeFrom(t, rows, func(ds *DuckDBShard, dir string) (string, error) {
					path, _, err := ds.exportParquet(ctx, dir, 0)
					return path, err
				})
			}},
			{Format: parquet.FormatTime, Write: func(t *testing.T, rows []*pb.LogRow) string {
				return writeFrom(t, rows, func(ds *DuckDBShard, dir string) (string, error) {
					path, _, err := ds.exportParquet(ctx, dir, 2)
					return path, err
				})
			}},
			{Format: parquet.FormatSized, Write: func(t *testing.T, rows []*pb.LogRow) string {
				return writeFrom(t, rows, func(ds *DuckDBShard, dir string) (string, error) {
					path, _, err := ds.exportKeyedParquet(ctx, dir, 0)
					return path, err
				})
			}},
		},
		Reader:    LocalReader(nil),
		Layout:    shape.BuildLayout(),
		SelectAll: "SELECT raw, ts FROM logs ORDER BY ts",
	})
}
