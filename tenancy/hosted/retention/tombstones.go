package retention

import (
	"context"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/logdb"
)

// ParquetTombstone is a superseded or retired parquet object awaiting GC.
// A separate table (not columns on shard_records) so it outlives the
// shard row when a fully-expired shard is retired to GONE.
type ParquetTombstone struct {
	ID          int64     `gorm:"primaryKey"`
	Path        string    `gorm:"column:path"`
	ShardID     string    `gorm:"column:shard_id"`
	Reason      string    `gorm:"column:reason"`
	DeleteAfter time.Time `gorm:"column:delete_after"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (*ParquetTombstone) TableName() string { return "parquet_tombstones" }

// SweepTombstones deletes every parquet object whose grace window has
// elapsed, then drops its tombstone row. S3 delete precedes the row
// delete, so a crash between them leaves the tombstone for the next
// sweep to retry (re-deleting a gone object is a no-op) — never a dropped
// row pointing at a live object. Per-object failures are logged and left
// for retry; the sweep continues. Returns the count deleted.
func (s Store) SweepTombstones(now time.Time, deleter logdb.ObjectDeleter) (int, error) {
	var tombs []ParquetTombstone
	if err := s.DB().Where("delete_after < ?", now).Find(&tombs).Error; err != nil {
		return 0, err
	}
	deleted := 0
	for _, t := range tombs {
		if err := deleter.DeleteParquet(t.Path); err != nil {
			weftsdk.Infof(context.Background(), "tombstone GC: delete %s (shard %s): %v", t.Path, t.ShardID, err)
			continue
		}
		if err := s.DB().Delete(&ParquetTombstone{}, t.ID).Error; err != nil {
			weftsdk.Infof(context.Background(), "tombstone GC: drop tombstone %d: %v", t.ID, err)
			continue
		}
		deleted++
	}
	return deleted, nil
}

// TombstoneSweeper is the daily GC of the parquet the janitor tombstones when
// it rewrites or retires an archive.
type TombstoneSweeper struct {
	Store   Store
	Deleter logdb.ObjectDeleter
	Clock   func() time.Time
	// Interval is the sweep cadence (24h in production).
	Interval time.Duration
	// IsLeader gates the pass to one coordinator. Nil means always run.
	IsLeader func() bool
}

// RunOnce sweeps the tombstones that are due, if this process leads.
func (t *TombstoneSweeper) RunOnce() {
	if t.IsLeader != nil && !t.IsLeader() {
		return
	}
	if _, err := t.Store.SweepTombstones(t.Clock(), t.Deleter); err != nil {
		weftsdk.Infof(context.Background(), "tombstone GC: sweep: %v", err)
	}
}

// Start runs RunOnce every Interval until ctx is cancelled. The first pass
// waits one interval: GC isn't urgent, and boot is busy.
func (t *TombstoneSweeper) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(t.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.RunOnce()
			}
		}
	}()
}

// startTombstoneSweeper starts the daily GC of tombstoned parquet. The
// objects go from the local disk, or from S3 when the archives live there.
func startTombstoneSweeper(ctx context.Context, prov *logdb.Provisioner, region string) {
	var deleter logdb.ObjectDeleter = logdb.LocalFileDeleter{}
	if strings.HasPrefix(prov.ParquetDir(), "s3://") {
		d, err := logdb.NewS3ObjectDeleter(ctx, region)
		if err != nil {
			weftsdk.Infof(ctx, "tombstone GC: S3 deleter unavailable, not starting: %v", err)
			return
		}
		deleter = d
	}
	(&TombstoneSweeper{
		Store:    Store{prov.ShardStore()},
		Deleter:  deleter,
		Clock:    time.Now,
		Interval: 24 * time.Hour,
		IsLeader: prov.IsLeader,
	}).Start(ctx)
	weftsdk.Infof(ctx, "coordinator: tombstone GC started (daily)")
}
