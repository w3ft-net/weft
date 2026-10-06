package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shard"
)

// runExportCommand is `weft-shard export`: the ephemeral archive export (see
// docs/plans/ephemeral-archive-export.md). It opens a shard database restored
// from a snapshot, exports it to Parquet under --prefix, and writes the
// completion manifest last. It never serves, heartbeats or registers, so the
// export host cannot be mistaken for a shard. Returns the process exit code:
// 0 on success, 2 for a usage error, 1 for a failed export.
func runExportCommand(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	shardID := fs.String("shard-id", "", "id of the shard being exported")
	dbPath := fs.String("db", "", "path to the restored shard.duckdb")
	prefix := fs.String("prefix", "", "archive prefix (s3://bucket/dir, or a local directory in dev)")
	attempt := fs.Int("attempt", 0, "coordinator's export attempt number, echoed in the manifest")
	timeout := fs.Duration("timeout", 2*time.Hour, "hard limit for the whole export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *shardID == "" || *dbPath == "" || *prefix == "" || *attempt <= 0 {
		fmt.Fprintln(os.Stderr, "usage: weft-shard export --shard-id ID --db PATH --prefix PREFIX --attempt N [--timeout D]")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := exportManifestStore(ctx, *prefix)
	if err != nil {
		log.Printf("export shard=%s attempt=%d: %v", *shardID, *attempt, err)
		return 1
	}
	m, err := shard.RunExport(ctx, shard.ExportConfig{
		ShardID: *shardID, DBPath: *dbPath, S3Prefix: *prefix, Attempt: *attempt,
	}, store)
	if err != nil {
		log.Printf("export shard=%s attempt=%d failed: %v", *shardID, *attempt, err)
		return 1
	}
	log.Printf("export shard=%s attempt=%d done: path=%s rows=%d db_bytes=%d took=%s",
		m.ShardID, m.Attempt, m.ParquetPath, m.Rows, m.DBBytes, m.Finished.Sub(m.Started).Round(time.Second))
	return 0
}

// exportManifestStore picks S3 for an s3:// prefix and the local filesystem
// otherwise, matching how Archive itself chooses its destination.
func exportManifestStore(ctx context.Context, prefix string) (shard.ManifestStore, error) {
	if !strings.HasPrefix(prefix, "s3://") {
		return shard.LocalManifestStore{}, nil
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}
	return shard.NewS3ManifestStore(ctx, region)
}
