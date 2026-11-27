package server

import (
	"context"
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

func exportCLIFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shard.duckdb")
	s, err := shard.NewDuckDBShard("s-cli", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(context.Background(), []*pb.LogRow{{Raw: "x", CustomerId: 1, TsUnixNs: int64(1_700_000_000) * int64(time.Second)}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunExportCommand_WritesParquetAndManifest(t *testing.T) {
	db := exportCLIFixture(t)
	out := t.TempDir()

	code := runExportCommand([]string{"--shard-id", "s-cli", "--db", db, "--prefix", out, "--attempt", "2", "--timeout", "1m"}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(out, "_manifests", "s-cli.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(out, "*.parquet")); len(m) != 1 {
		t.Fatalf("parquet files = %v, want 1", m)
	}
}

func TestRunExportCommand_FailureExitsNonZeroWithoutManifest(t *testing.T) {
	out := t.TempDir()
	missing := filepath.Join(t.TempDir(), "nope.duckdb")

	code := runExportCommand([]string{"--shard-id", "s-cli", "--db", missing, "--prefix", out, "--attempt", "1"}, nil)
	if code == 0 {
		t.Fatal("exit code 0 for a missing database")
	}
	if _, err := os.Stat(filepath.Join(out, "_manifests", "s-cli.json")); err == nil {
		t.Fatal("manifest written for a failed export")
	}
}

func TestRunExportCommand_RequiresFlags(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--shard-id", "s", "--db", "/x"}, // no prefix
		{"--shard-id", "s", "--prefix", "/x", "--attempt", "1"}, // no db
		{"--db", "/x", "--prefix", "/x", "--attempt", "1"},      // no shard id
		{"--shard-id", "s", "--db", "/x", "--prefix", "/x"},     // no attempt
	} {
		if code := runExportCommand(args, nil); code != 2 {
			t.Errorf("args %v: exit code = %d, want 2 (usage)", args, code)
		}
	}
}
