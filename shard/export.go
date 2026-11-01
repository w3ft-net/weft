package shard

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
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

// ManifestReader is the coordinator's side: it polls for the manifest an export
// wrote. found is false (with a nil error) when no manifest exists yet; an
// unreadable or corrupt manifest is an error, never "not found".
type ManifestReader interface {
	Get(ctx context.Context, key string) (m ExportManifest, found bool, err error)
}

// ExportOutputSuffix names one attempt's output: the export writes
// <prefix>/<shard-id>.attempt-N[.parquet], never into the location a failed
// attempt used. DuckDB's OVERWRITE_OR_IGNORE only replaces same-named files, so
// a retry that shared a directory with a partial earlier attempt would leave the
// old files in place and the archive glob would read both (rows double-count).
func ExportOutputSuffix(attempt int) string {
	return fmt.Sprintf(".attempt-%d", attempt)
}

// ExportOutputPaths lists the delete targets for one attempt's output: the
// hive-partitioned directory form and the single-file form, in the shapes
// logdb.ObjectDeleter takes. They are exact (the directory ends in a slash
// before its glob, the file in .parquet), so attempt 1 can never match
// attempt 10.
func ExportOutputPaths(prefix, shardID string, attempt int) []string {
	base := strings.TrimRight(prefix, "/") + "/" + shardID + ExportOutputSuffix(attempt)
	return []string{base + "/**/*.parquet", base + ".parquet"}
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
	s := &DuckDBShard{id: id, dbPath: path, db: db}
	// A database with a table per key must be exported from those tables: its
	// logs table is an empty template, and exporting that would report success.
	if err := s.initLayout(false); err != nil {
		db.Close()
		return nil, fmt.Errorf("export database %q: %w", path, err)
	}
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + s.source()).Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("export database %q has no readable logs table: %w", path, err)
	}
	return s, nil
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
	path, stats, err := s.archive(ctx, cfg.S3Prefix, ExportOutputSuffix(cfg.Attempt))
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

func (LocalManifestStore) Get(_ context.Context, key string) (ExportManifest, bool, error) {
	b, err := os.ReadFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return ExportManifest{}, false, nil
	}
	if err != nil {
		return ExportManifest{}, false, err
	}
	return decodeManifest(key, b)
}

func decodeManifest(key string, b []byte) (ExportManifest, bool, error) {
	var m ExportManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return ExportManifest{}, false, fmt.Errorf("decode manifest %s: %w", key, err)
	}
	return m, true, nil
}

// S3ManifestStore reads and writes manifests in S3. A single PutObject is atomic: readers
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

func (s *S3ManifestStore) Get(ctx context.Context, key string) (ExportManifest, bool, error) {
	bucket, objKey, err := parseS3URL(key)
	if err != nil {
		return ExportManifest{}, false, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &objKey})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return ExportManifest{}, false, nil
		}
		return ExportManifest{}, false, fmt.Errorf("get manifest %s: %w", key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return ExportManifest{}, false, fmt.Errorf("read manifest %s: %w", key, err)
	}
	return decodeManifest(key, b)
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
