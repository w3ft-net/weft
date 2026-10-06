package shard

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The ephemeral archive export (docs/plans/ephemeral-archive-export.md): a
// short-lived instance mounts a volume restored from a snapshot of the shard's
// volume, exports it to Parquet, and writes a manifest last. The coordinator
// treats the manifest as proof the export is complete, so it is only ever
// written after the Parquet output succeeded.

// ExportConfig describes one export attempt.
type ExportConfig struct {
	ShardID  string
	DBPath   string // the restored shard.duckdb
	S3Prefix string // archive prefix, same value the in-process Archive receives
	Attempt  int    // the coordinator's attempt number, echoed in the manifest
}

// ExportManifest is the completion record an export writes to the archive
// prefix. Attempt lets the coordinator ignore a stale manifest left by an
// earlier attempt.
type ExportManifest struct {
	ShardID     string         `json:"shard_id"`
	Attempt     int            `json:"attempt"`
	ParquetPath string         `json:"parquet_path"`
	Rows        int64          `json:"rows"`
	DBBytes     int64          `json:"db_bytes"`
	Customers   []CustomerStat `json:"customers"`
	Started     time.Time      `json:"started"`
	Finished    time.Time      `json:"finished"`
}

// ManifestStore persists a manifest at key (as produced by ManifestKey).
type ManifestStore interface {
	Put(ctx context.Context, key string, m ExportManifest) error
}

// ManifestKey is where a shard's export manifest lives under the archive
// prefix. The coordinator derives the same key to poll for completion.
func ManifestKey(prefix, shardID string) string {
	return strings.TrimRight(prefix, "/") + "/_manifests/" + shardID + ".json"
}

// OpenForExport opens an existing shard database for export without touching
// its schema: no table creation, no migrations. It refuses a missing file or a
// database without a logs table, because a mis-mounted or empty volume must
// fail the export, not produce a "successful" empty archive that leads the
// coordinator to retire the real volume.
func OpenForExport(id, path string) (*DuckDBShard, error) {
	// sql.Open on a missing path would silently create a new empty database.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("export database: %w", err)
	}
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("export database %q has no readable logs table: %w", path, err)
	}
	return &DuckDBShard{id: id, dbPath: path, db: db}, nil
}

// RunExport exports cfg.DBPath to Parquet under cfg.S3Prefix, then writes the
// manifest. A failed export writes no manifest. A failed manifest write fails
// the export: without the manifest the coordinator cannot know it finished.
func RunExport(ctx context.Context, cfg ExportConfig, store ManifestStore) (ExportManifest, error) {
	s, err := OpenForExport(cfg.ShardID, cfg.DBPath)
	if err != nil {
		return ExportManifest{}, err
	}
	defer s.Close()

	m := ExportManifest{ShardID: cfg.ShardID, Attempt: cfg.Attempt, Started: time.Now().UTC()}
	if fi, err := os.Stat(cfg.DBPath); err == nil {
		m.DBBytes = fi.Size()
	}
	path, stats, err := s.Archive(ctx, cfg.S3Prefix)
	if err != nil {
		return ExportManifest{}, err
	}
	m.ParquetPath = path
	m.Customers = stats
	for _, c := range stats {
		m.Rows += c.RowCount
	}
	m.Finished = time.Now().UTC()

	if err := store.Put(ctx, ManifestKey(cfg.S3Prefix, cfg.ShardID), m); err != nil {
		return ExportManifest{}, fmt.Errorf("write manifest: %w", err)
	}
	return m, nil
}

// LocalManifestStore writes manifests to the local filesystem (dev and tests).
// The write is atomic so a reader never sees a partial manifest.
type LocalManifestStore struct{}

func (LocalManifestStore) Put(_ context.Context, key string, m ExportManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(key), 0755); err != nil {
		return err
	}
	tmp := key + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, key)
}

// S3ManifestStore writes manifests to S3. A single PutObject is atomic: readers
// see the whole object or none of it.
type S3ManifestStore struct {
	client *s3.Client
}

// NewS3ManifestStore builds a store from the default credential chain (the
// export instance's IAM role).
func NewS3ManifestStore(ctx context.Context, region string) (*S3ManifestStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &S3ManifestStore{client: s3.NewFromConfig(cfg)}, nil
}

func (s *S3ManifestStore) Put(ctx context.Context, key string, m ExportManifest) error {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ct := "application/json"
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: &objKey, Body: bytes.NewReader(b), ContentType: &ct,
	})
	return err
}

// parseS3URL splits "s3://bucket/key" into its bucket and key.
func parseS3URL(u string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(u, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3:// url: %q", u)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("s3 url needs a bucket and a key: %q", u)
	}
	return bucket, key, nil
}
