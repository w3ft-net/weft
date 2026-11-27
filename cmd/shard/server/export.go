package server

import (
	"context"
	"flag"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"os"
	"strings"
	"time"

	"github.com/w3ft-net/weft/shard"
)

// runExportCommand is `weft-shard export`: the ephemeral archive export. It opens a shard database restored
// from a snapshot, exports it to Parquet under --prefix, and writes the
// completion manifest last. It never serves, heartbeats or registers, so the
// export host cannot be mistaken for a shard. Returns the process exit code:
// 0 on success, 2 for a usage error, 1 for a failed export.
func runExportCommand(args []string, manifests func(context.Context, string) (shard.ManifestStore, error)) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	shardID := fs.String("shard-id", "", "id of the shard being exported")
	dbPath := fs.String("db", "", "path to the restored shard.duckdb")
	prefix := fs.String("prefix", "", "archive prefix (s3://bucket/dir, or a local directory in dev)")
	attempt := fs.Int("attempt", 0, "coordinator's export attempt number, echoed in the manifest")
	keyed := fs.Bool("keyed", false, "split a large archive by log key as well as by time")
	timeout := fs.Duration("timeout", 2*time.Hour, "hard limit for the whole export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *shardID == "" || *dbPath == "" || *prefix == "" || *attempt <= 0 {
		fmt.Fprintln(os.Stderr, "usage: weft-shard export --shard-id ID --db PATH --prefix PREFIX --attempt N [--keyed] [--timeout D]")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := exportManifestStore(ctx, *prefix, manifests)
	if err != nil {
		weftsdk.Infof(ctx, "export shard=%s attempt=%d: %v", *shardID, *attempt, err)
		return 1
	}
	m, err := shard.RunExport(ctx, shard.ExportConfig{
		ShardID: *shardID, DBPath: *dbPath, S3Prefix: *prefix, Attempt: *attempt, Keyed: *keyed,
	}, store)
	if err != nil {
		weftsdk.Errorf(ctx, "export shard=%s attempt=%d failed: %v", *shardID, *attempt, err)
		return 1
	}
	weftsdk.Infof(ctx, "export shard=%s attempt=%d done: path=%s rows=%d db_bytes=%d took=%s",
		m.ShardID, m.Attempt, m.ParquetPath, m.Rows, m.DBBytes, m.Finished.Sub(m.Started).Round(time.Second))
	return 0
}

// exportManifestStore is the local filesystem for a directory prefix, and the
// deployment's store for anything else (an s3:// prefix): without one an
// object-store prefix is refused.
func exportManifestStore(ctx context.Context, prefix string, manifests func(context.Context, string) (shard.ManifestStore, error)) (shard.ManifestStore, error) {
	if !strings.Contains(prefix, "://") {
		return shard.LocalManifestStore{}, nil
	}
	if manifests == nil {
		return nil, fmt.Errorf("archive prefix %q needs a manifest store, and this build links none", prefix)
	}
	return manifests(ctx, prefix)
}
