//go:build integration && test

package integration

import (
	"fmt"
	"os"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

func TestMain(m *testing.M) {
	cleanup := Setup()
	defer cleanup()
	os.Exit(m.Run())
}

// Relative to time.Now(), not a fixed calendar date — see
// coordinator/coordinator_test.go's t0/t1/t2 comment for why a fixed
// date here eventually falls outside every plan tier's retention
// clamp as real time advances.
var (
	t0 = time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	t1 = t0.Add(1 * time.Hour)
)

func TestExportToS3_DirectWrite(t *testing.T) {
	ds, err := shard.NewDuckDBShard("s3-export", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "direct s3 export row 1", Namespace: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "direct s3 export row 2", Namespace: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{Raw: "direct s3 export row 3", Namespace: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows, nil); err != nil {
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
	cs, err := shard.NewColdShard("cold-s3-direct", []string{"vimgym", "callbox"}, s3Path, "", s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	result, err := cs.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
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
		{Raw: "vimgym log", Namespace: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "vimgym error", Namespace: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
	}, nil)
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
		{Raw: "callbox log", Namespace: "callbox", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
	}, nil)
	cbPath := fmt.Sprintf("s3://%s/kevlar/callbox/2026/03/shard-callbox.parquet", ColdBucket)
	if err := dsCb.ExportToS3Parquet(cbPath, s3cfg); err != nil {
		t.Fatal(err)
	}
	dsCb.Close()

	t.Logf("exported to %s and %s", vimPath, cbPath)

	// Query each independently.
	csVim, err := shard.NewColdShard("cold-vim", []string{"vimgym"}, vimPath, "", s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer csVim.Close()

	result, err := csVim.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym rows, got %d", len(result))
	}

	csCb, err := shard.NewColdShard("cold-cb", []string{"callbox"}, cbPath, "", s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer csCb.Close()

	result, err = csCb.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
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
	if cfg.Endpoint != S3Endpoint {
		t.Errorf("expected endpoint %s, got %s", S3Endpoint, cfg.Endpoint)
	}
	if cfg.AccessKey != S3AccessKey {
		t.Errorf("expected access key %s, got %s", S3AccessKey, cfg.AccessKey)
	}
}
