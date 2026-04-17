package shard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// identityFilename is the on-disk filename for a shard's durable identity.
// Lives in the shard's data directory (same dir as shard.duckdb), so it
// rides on the EBS volume.
const identityFilename = "shard.json"

// Identity is the durable record of a shard's name, window, and app set.
// Written by the Configure RPC, read by the shard process at startup.
// Source of truth when an instance restarts against an existing EBS volume.
type Identity struct {
	ShardID     string    `json:"shard_id"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Apps        []string  `json:"apps"`
}

// LoadIdentity reads shard.json from dataDir. Returns (nil, nil) when the
// file is absent — that's the "fresh EBS, needs christening" signal.
// A present-but-malformed file is an error: treating a corrupted identity
// as "fresh" would cause the coordinator to mint a second shard id on the
// same volume and fork history.
func LoadIdentity(dataDir string) (*Identity, error) {
	path := filepath.Join(dataDir, identityFilename)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if id.ShardID == "" {
		return nil, fmt.Errorf("%s: shard_id empty", path)
	}
	return &id, nil
}

// SaveIdentity writes shard.json atomically — write to a temp file in the
// same directory, fsync, rename into place. A crash mid-write leaves the
// previous (or no) file intact rather than a half-written one.
func SaveIdentity(dataDir string, id *Identity) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	final := filepath.Join(dataDir, identityFilename)
	tmp, err := os.CreateTemp(dataDir, identityFilename+".*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("rename into %s: %w", final, err)
	}
	return nil
}
