package logdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"gorm.io/gorm"
)

// ErrArchivePathChanged means a shard's archive is not at the path a swap named,
// so the swap was not made.
var ErrArchivePathChanged = errors.New("the shard's archive is not at the path given")

// ArchiveSwapGCDelay is how long the archive a swap replaced is kept before the
// tombstone GC deletes it: a week, long enough to notice a bad conversion and
// swap back by hand.
const ArchiveSwapGCDelay = 7 * 24 * time.Hour

// SwapArchivePath moves a shard's archive from oldPath to newPath and tombstones
// oldPath, in one transaction, only if the shard is still at oldPath. A shard
// already at newPath is left as it is and is not an error, so a swap told to
// every coordinator succeeds on all of them.
func (s ShardStore) SwapArchivePath(shardID, oldPath, newPath string, now time.Time, gcDelay time.Duration) error {
	return s.db().Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Shard{}).Where("id = ? AND parquet_path = ?", shardID, oldPath).Update("parquet_path", newPath)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var cur Shard
			err := tx.Where("id = ?", shardID).First(&cur).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("shard %s: not found", shardID)
			}
			if err != nil {
				return err
			}
			if cur.ParquetPath == newPath {
				return nil
			}
			return fmt.Errorf("shard %s is at %q: %w", shardID, cur.ParquetPath, ErrArchivePathChanged)
		}
		return tx.Create(&ParquetTombstone{
			Path: oldPath, ShardID: shardID, Reason: "swap", DeleteAfter: now.Add(gcDelay),
		}).Error
	})
}

// SwapArchive points an ARCHIVED shard at a converted archive: the database
// (SwapArchivePath) and then this coordinator's live registry, so queries read
// the new archive from the next one on. Another coordinator's registry follows
// when it is told the same, or at its next boot.
func (p *Provisioner) SwapArchive(shardID, oldPath, newPath string) error {
	entry := p.registry.Get(shardID)
	if entry == nil {
		return fmt.Errorf("shard %s: %w", shardID, ErrShardNotInRegistry)
	}
	entry.mu.RLock()
	st := entry.State
	entry.mu.RUnlock()
	if st != StateArchived {
		return fmt.Errorf("shard %s is %s, not ARCHIVED: %w", shardID, st, ErrShardWrongState)
	}
	if p.store == nil {
		return errors.New("no store")
	}
	if err := (ShardStore{p.store}).SwapArchivePath(shardID, oldPath, newPath, time.Now(), ArchiveSwapGCDelay); err != nil {
		return err
	}
	p.registry.RefreshArchivedPath(shardID, newPath, p.config.LambdaFunctionURL)
	weftsdk.Infof(context.Background(), "provisioner: archive of %s swapped from %s to %s", shardID, oldPath, newPath)
	return nil
}
