package integration

import (
	"fmt"
	"os"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func TestMain(m *testing.M) {
	cleanup := Setup()
	defer cleanup()
	os.Exit(m.Run())
}

var (
	t0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC)
)

func TestExportToS3_DirectWrite(t *testing.T) {
	ds, err := shard.NewDuckDBShard("s3-export", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{LineNum: 1, Raw: "direct s3 export row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{LineNum: 2, Raw: "direct s3 export row 2", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{LineNum: 3, Raw: "direct s3 export row 3", App: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows); err != nil {
		t.Fatal(err)
	}

	// Export directly to S3 via DuckDB httpfs.
	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set — harness should have configured env vars")
	}

	s3Path := fmt.Sprintf("s3://%s/test/direct-export.parquet", ColdBucket)
	if err := ds.ExportToS3Parquet(s3Path, s3cfg); err != nil {
		t.Fatalf("export to S3: %v", err)
	}
	t.Logf("exported to %s", s3Path)

	// Read it back via ColdShard.
	cs, err := shard.NewColdShard("cold-s3-direct", []string{"vimgym", "callbox"}, s3Path, s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	result, err := cs.Query("SELECT * FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 rows from S3, got %d", len(result))
	}
}

func TestExportToS3_AppPartitioned(t *testing.T) {
	// Export two app-scoped Parquet files to S3.
	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set")
	}

	// Vimgym shard.
	dsVim, err := shard.NewDuckDBShard("vim-export", "")
	if err != nil {
		t.Fatal(err)
	}
	dsVim.Ingest([]*pb.LogRow{
		{LineNum: 1, Raw: "vimgym log", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{LineNum: 2, Raw: "vimgym error", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
	})
	vimPath := fmt.Sprintf("s3://%s/kevlar/vimgym/2026/03/shard-vimgym.parquet", ColdBucket)
	if err := dsVim.ExportToS3Parquet(vimPath, s3cfg); err != nil {
		t.Fatal(err)
	}
	dsVim.Close()

	// Callbox shard.
	dsCb, err := shard.NewDuckDBShard("cb-export", "")
	if err != nil {
		t.Fatal(err)
	}
	dsCb.Ingest([]*pb.LogRow{
		{LineNum: 3, Raw: "callbox log", App: "callbox", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	})
	cbPath := fmt.Sprintf("s3://%s/kevlar/callbox/2026/03/shard-callbox.parquet", ColdBucket)
	if err := dsCb.ExportToS3Parquet(cbPath, s3cfg); err != nil {
		t.Fatal(err)
	}
	dsCb.Close()

	t.Logf("exported to %s and %s", vimPath, cbPath)

	// Query each independently.
	csVim, err := shard.NewColdShard("cold-vim", []string{"vimgym"}, vimPath, s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer csVim.Close()

	result, err := csVim.Query("SELECT * FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym rows, got %d", len(result))
	}

	csCb, err := shard.NewColdShard("cold-cb", []string{"callbox"}, cbPath, s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer csCb.Close()

	result, err = csCb.Query("SELECT * FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 callbox row, got %d", len(result))
	}
}

func TestS3ConfigFromEnv(t *testing.T) {
	cfg := shard.S3ConfigFromEnv()
	if cfg == nil {
		t.Fatal("expected S3Config from env vars")
	}
	if cfg.Endpoint != MinioEndpoint {
		t.Errorf("expected endpoint %s, got %s", MinioEndpoint, cfg.Endpoint)
	}
	if cfg.AccessKey != MinioAccessKey {
		t.Errorf("expected access key %s, got %s", MinioAccessKey, cfg.AccessKey)
	}
}
