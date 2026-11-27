package shard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// The ephemeral archive export: a
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
	Keyed    bool   // split a partitioned archive by log key too (see IsKeyed)
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
	// The archive is written in the layout the data was ingested with, which
	// the volume records in shard.json beside the database. A volume with no
	// shard.json (a dev database) has the legacy layout.
	layout := shape.Legacy()
	ident, err := LoadIdentity(filepath.Dir(path))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("export database %q: %w", path, err)
	}
	if ident != nil {
		layout = ident.Layout
	}
	s := &DuckDBShard{id: id, dbPath: path, db: db, layout: layout}
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
	path, stats, err := s.archive(ctx, cfg.S3Prefix, ExportOutputSuffix(cfg.Attempt), cfg.Keyed)
	if err != nil {
		return ExportManifest{}, err
	}
	m.ParquetPath = path
	m.Partitions = stats
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
	return DecodeManifest(key, b)
}

// DecodeManifest reads a stored manifest, found false for an empty one; key names it in errors.
func DecodeManifest(key string, b []byte) (ExportManifest, bool, error) {
	var m ExportManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return ExportManifest{}, false, fmt.Errorf("decode manifest %s: %w", key, err)
	}
	return m, true, nil
}
