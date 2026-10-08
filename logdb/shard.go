package logdb

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// shardRowStore is the shard_records persistence surface the shard SM
// needs (ShardSMDeps.Store). Declared as an interface — not the concrete
// ShardStore — so deps.Store stays nilable for the dev/test paths that
// don't wire persistence, and so tests can substitute a fake.
type shardRowStore interface {
	GetShard(id string) (*Shard, bool, error)
	DeleteShard(id string) error
	SetShardState(id string, state ShardState) error
	SetShardAddr(id, addr string) error
	SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error
	IncrementShardLaunchAttempts(id string) error
	SetShardParquetPath(id, path string) error
	RecordArchiveStats(id string, stats []shard.CustomerStat) error
	ClearShardVolume(id string) error
	ClearShardInstance(id string) error
	LatestPreRecoveryState(id string) (ShardState, error)

	// Ephemeral archive export facts (migration 080).
	BeginExportAttempt(id string, prevAttempt int, instanceType string, startedAt time.Time, maxActive int) (ExportClaim, error)
	SetExportSnapshot(id, snapshotID string) error
	SetExportInstance(id, instanceID string) error
	ClearExportFacts(id string) error
	LatestShardDiskBytes(id string) (int64, error)
}

// ShardArchiveStat is one (shard, customer) row of archive metadata: the
// customer's row count and ts span within an archived shard's parquet.
// Captured at archive time (from the Archive RPC's customer_stats) and
// consulted by the retention janitor to classify archived files
// (delete / keep / rewrite) without scanning S3. See
// docs/plans/retention-janitor.md.
type ShardArchiveStat struct {
	ShardID  string      `gorm:"primaryKey;column:shard_id"`
	OrgID    store.OrgID `gorm:"primaryKey;column:org_id"`
	MinTs    time.Time   `gorm:"not null;column:min_ts"`
	MaxTs    time.Time   `gorm:"not null;column:max_ts"`
	RowCount int64       `gorm:"not null;column:row_count"`
}

func (*ShardArchiveStat) TableName() string { return "shard_archive_stats" }

// ShardStore is the gorm-backed persistence for the unified Shard type.
// It embeds *store.Store — so it inherits the shared DB connection,
// tenant-scoping, and every non-shard-row method (shard_services,
// heartbeats, buckets) — and adds the shard_records row operations that
// used to live on *store.Store as methods over store.ShardRecord. They
// moved here because the row model is now logdb.Shard, and store importing
// logdb would be an import cycle (logdb imports store). The DB schema is
// unchanged; only the Go ownership of the model + its row methods moved.
//
// ShardStore satisfies both RegistryStore and the provisioner's
// ShardOpsStore / ShardSMDeps.Store needs.
type ShardStore struct {
	*store.Store
}

// db is the shared *gorm.DB behind the embedded *store.Store.
func (s ShardStore) db() *gorm.DB { return s.DB() }

// SaveShard upserts a shard row. The caller passes a snapshot copy (see
// ShardRegistry.persistedCopy), so gorm's reflection over the struct
// can't race the registry's live entry.
func (s ShardStore) SaveShard(sh *Shard) error {
	return s.db().Save(sh).Error
}

// GetShard returns the row for id. (row, true, nil) when found,
// (nil, false, nil) when absent, (nil, false, err) on DB error. Used by
// the shard SM's state accessor and the adopt path.
func (s ShardStore) GetShard(id string) (*Shard, bool, error) {
	var sh Shard
	err := s.db().Where("id = ?", id).First(&sh).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &sh, true, nil
}

// ListShards returns every shard row as fresh *Shard objects (loaded
// independently of the registry's live entries).
func (s ShardStore) ListShards() ([]*Shard, error) {
	var shards []*Shard
	err := s.db().Find(&shards).Error
	return shards, err
}

// ListShardsByBucket returns a bucket's shard rows, newest window first —
// the read behind the admin shards listing. Reads straight from
// shard_records (not the registry) so archived shards still appear.
func (s ShardStore) ListShardsByBucket(bucketID int64) ([]*Shard, error) {
	var shards []*Shard
	err := s.db().Where("bucket_id = ?", bucketID).Order("window_start DESC").Find(&shards).Error
	return shards, err
}

// DeleteShard removes a shard row.
func (s ShardStore) DeleteShard(id string) error {
	return s.db().Delete(&Shard{}, "id = ?", id).Error
}

// TouchHeartbeat updates the heartbeat-owned fields (row_count, disk_bytes,
// last_heartbeat_at, binary_sha) and nothing else — window bounds, state,
// and services are left untouched so manual DB edits or reconciler-driven
// changes aren't clobbered. Written via a map (not a struct) so a zero
// row_count still persists. at is the value the SM stamped from deps.Clock(),
// so the column matches the in-memory LastHeartbeatAt that isStale()
// consults at runtime. binarySHA is the shard's compile-time GitSHA — what
// weft-admin reads to skip rolling a shard already on the target binary.
// diskBytes is only written when positive: a beat that doesn't report a
// footprint must not erase the last known one, which sizes the export host.
func (s ShardStore) TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error {
	cols := map[string]any{
		"row_count":         rowCount,
		"last_heartbeat_at": at,
		"binary_sha":        binarySHA,
	}
	if diskBytes > 0 {
		cols["disk_bytes"] = diskBytes
	}
	return s.db().Model(&Shard{}).Where("id = ?", id).Updates(cols).Error
}

// SetShardLaunchResult persists the identifiers a successful launch
// produced (instance, AZ, volume, stripe index). Narrow on purpose —
// Select names the four columns so a zero value (e.g. an empty instance)
// still writes, but nothing else (state, heartbeat, window) is touched.
func (s ShardStore) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("InstanceID", "AZ", "EBSVolumeID", "StripeIndex").
		Updates(&Shard{
			InstanceID:  instanceID,
			AZ:          az,
			EBSVolumeID: volumeID,
			StripeIndex: stripeIndex,
		}).Error
}

// SetShardState narrow-writes only the state column — the shard FSM's
// transition mutator. Lifecycle-owned fields (window bounds, services,
// addr) move through SaveShard, so a transition can't trample a
// concurrent SaveShard over them.
func (s ShardStore) SetShardState(id string, state ShardState) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Update("state", string(state)).Error
}

// SetShardAddr persists the live gRPC address a heartbeat reported.
// Narrow (Select forces the single column, so an empty string still
// writes) so the heartbeat path can persist addr without a full SaveShard
// trampling lifecycle-owned fields. Persisting it is what lets
// ReconstructShard eager-dial the shard after a coordinator restart and
// stops RefreshFromStore from clobbering the live addr with a stale row.
func (s ShardStore) SetShardAddr(id, addr string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("Addr").Updates(&Shard{Addr: addr}).Error
}

// SetShardIngestFailures persists the consecutive-ingest-failure counter.
// Narrow (Select forces a zero value to write, e.g. a reset to 0) so it
// can't race a state write. Mirrors TouchHeartbeat: called synchronously
// from ShardRegistry.RecordIngestFailure/ResetIngestFailures so the
// ingest-side circuit breaker (evaluateIngesting's threshold check) stays
// meaningful once evaluateStateMachines is gated to a single leader that
// may not be the coordinator actually routing this shard's ingest writes.
// See docs/plans/coordinator-horizontal-scaling.md #5 "Real gap #2".
func (s ShardStore) SetShardIngestFailures(id string, n int) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("IngestFailures").Updates(&Shard{IngestFailures: n}).Error
}

// IncrementShardLaunchAttempts bumps launch_attempts by one.
func (s ShardStore) IncrementShardLaunchAttempts(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		UpdateColumn("launch_attempts", gorm.Expr("launch_attempts + 1")).Error
}

// SetShardParquetPath records where an archived shard's data landed in S3.
// Narrow (Select forces the single column) so it can't race a state write.
func (s ShardStore) SetShardParquetPath(id, path string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("ParquetPath").Updates(&Shard{ParquetPath: path}).Error
}

// RecordArchiveStats replaces the shard_archive_stats rows for a shard
// with a freshly-captured set. Replace-not-merge so a re-archival (a
// retry, or a future janitor rewrite that drops a customer's rows
// entirely) leaves no stale per-customer rows behind. A nil/empty set
// (a shard that archived with zero rows) clears any prior rows and adds
// none. The whole swap runs in one transaction so a concurrent janitor
// read never sees the shard mid-delete.
func (s ShardStore) RecordArchiveStats(id string, stats []shard.CustomerStat) error {
	return s.db().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("shard_id = ?", id).Delete(&ShardArchiveStat{}).Error; err != nil {
			return err
		}
		if len(stats) == 0 {
			return nil
		}
		rows := make([]ShardArchiveStat, len(stats))
		for i, st := range stats {
			rows[i] = ShardArchiveStat{
				ShardID:  id,
				OrgID:    store.OrgID(st.CustomerID),
				MinTs:    st.MinTs,
				MaxTs:    st.MaxTs,
				RowCount: st.RowCount,
			}
		}
		return tx.Create(&rows).Error
	})
}

// ClearShardVolume blanks ebs_volume_id after an archived shard's data
// volume is snapshotted and deleted. Select forces the empty string to
// write (a zero-value struct field is otherwise skipped by gorm).
func (s ShardStore) ClearShardVolume(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("EBSVolumeID").Updates(&Shard{EBSVolumeID: ""}).Error
}

// ClearShardInstance blanks instance_id after an archived shard's
// instance is terminated. Same narrow-write rationale as ClearShardVolume.
func (s ShardStore) ClearShardInstance(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Select("InstanceID").Updates(&Shard{InstanceID: ""}).Error
}

// ExportClaim is the outcome of BeginExportAttempt.
type ExportClaim int

const (
	// ExportClaimWon: this caller started the shard's next attempt.
	ExportClaimWon ExportClaim = iota
	// ExportClaimLost: another evaluator already claimed this shard's next
	// attempt (a leader handover), or the shard row is gone.
	ExportClaimLost
	// ExportClaimHeld: the cap on concurrent attempts is reached. Nothing was
	// written: no attempt number was taken and no clock started.
	ExportClaimHeld
)

// exportClaimLockKey serializes every export claim in the cluster (an
// arbitrary constant; "WeFt").
const exportClaimLockKey int64 = 0x57654674

// BeginExportAttempt claims the next export attempt for a shard: it moves
// export_attempt from prevAttempt to prevAttempt+1 and records the chosen
// instance type and start time, provided export_attempt is still prevAttempt
// (else ExportClaimLost, so two evaluators racing across a leader handover
// cannot both start the same attempt) and fewer than maxActive other shards
// have an attempt in flight (else ExportClaimHeld).
//
// The cap is checked here, in the same transaction as the claim, because that
// is the only place it can be exact: shards are evaluated concurrently, so a
// count taken before a later write would let several of them pass it together,
// and a cap that only held the launch would leave a held shard's attempt clock
// running while it queued. A transaction-scoped advisory lock serializes the
// claims; "in flight" means export_started_at is set, which ClearExportFacts
// resets, so a finished or failed attempt gives its slot back.
func (s ShardStore) BeginExportAttempt(id string, prevAttempt int, instanceType string, startedAt time.Time, maxActive int) (ExportClaim, error) {
	claim := ExportClaimLost
	err := s.db().Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", exportClaimLockKey).Error; err != nil {
			return err
		}
		var current []int
		if err := tx.Model(&Shard{}).Where("id = ?", id).Pluck("export_attempt", &current).Error; err != nil {
			return err
		}
		if len(current) != 1 || current[0] != prevAttempt {
			return nil // lost: the row is gone, or someone else already claimed
		}
		var active int64
		if err := tx.Model(&Shard{}).Where("export_started_at IS NOT NULL AND id <> ?", id).Count(&active).Error; err != nil {
			return err
		}
		if int(active) >= maxActive {
			claim = ExportClaimHeld
			return nil
		}
		res := tx.Model(&Shard{}).Where("id = ? AND export_attempt = ?", id, prevAttempt).
			Updates(map[string]any{
				"export_attempt":       prevAttempt + 1,
				"export_instance_type": instanceType,
				"export_started_at":    startedAt,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 {
			claim = ExportClaimWon
		}
		return nil
	})
	return claim, err
}

// SetExportSnapshot records the snapshot taken for the current export attempt.
func (s ShardStore) SetExportSnapshot(id, snapshotID string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Update("export_snapshot_id", snapshotID).Error
}

// SetExportInstance records the export instance launched for the current attempt.
func (s ShardStore) SetExportInstance(id, instanceID string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Update("export_instance_id", instanceID).Error
}

// ClearExportFacts ends the current attempt's bookkeeping. It deliberately
// leaves export_attempt alone: the counter caps retries and keeps the next
// attempt's tags distinct from this one's.
func (s ShardStore) ClearExportFacts(id string) error {
	return s.db().Model(&Shard{}).Where("id = ?", id).
		Updates(map[string]any{
			"export_snapshot_id":   nil,
			"export_instance_id":   nil,
			"export_instance_type": nil,
			"export_started_at":    nil,
		}).Error
}

// StuckShard is a shard that has been ARCHIVING since Since.
type StuckShard struct {
	ShardID  string    `gorm:"column:shard_id"`
	Since    time.Time `gorm:"column:since"`
	Attempts int       `gorm:"column:attempts"`
}

// StuckArchiving returns the shards currently ARCHIVING whose most recent entry
// into ARCHIVING (from shard_operations, which the row trigger writes on every
// state change) is before the given time.
func (s ShardStore) StuckArchiving(before time.Time) ([]StuckShard, error) {
	var out []StuckShard
	err := s.db().Raw(`
		SELECT r.id AS shard_id, MAX(o.ts) AS since, r.export_attempt AS attempts
		FROM shard_records r
		JOIN shard_operations o ON o.shard_id = r.id AND o.to_state = 'ARCHIVING'
		WHERE r.state = 'ARCHIVING'
		GROUP BY r.id, r.export_attempt
		HAVING MAX(o.ts) < ?
		ORDER BY since`, before).Scan(&out).Error
	return out, err
}

// LatestShardDiskBytes returns the disk footprint (.duckdb + WAL + spill) from
// the shard's most recent heartbeat that reported one, or 0 if it never has.
// It sizes the export instance; the WAL and spill make it a slight
// overestimate of the .duckdb alone, which errs toward a bigger instance.
// Read from shard_records rather than shard_heartbeats: the heartbeat table
// is transient and pruned, and this must still answer long after the shard's
// instance stopped beating.
func (s ShardStore) LatestShardDiskBytes(id string) (int64, error) {
	var rows []struct{ DiskBytes int64 }
	err := s.db().Raw(`SELECT disk_bytes FROM shard_records WHERE id = ?`, id).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].DiskBytes, nil
}

// LatestPreRecoveryState returns the state the shard was in immediately
// before its most recent transition into a recovery state (UPGRADING or
// SUSPENDED). The shard SM uses this to route LAUNCHING → INGESTING /
// READ_ONLY / DEGRADED based on where the shard came from, so a roll of
// a READ_ONLY shard returns to READ_ONLY rather than bouncing through
// INGESTING and getting scale-down-rotated back. Returns empty ("") if
// the shard has no recovery history — the caller defaults to
// INGESTING (the fresh-launch path).
//
// Reads from shard_operations, which the row trigger writes on every
// state transition (see store/schema.sql). The (shard_id, ts) index
// makes this a fast bounded lookup.
func (s ShardStore) LatestPreRecoveryState(id string) (ShardState, error) {
	var result struct {
		FromState string
	}
	err := s.db().Raw(`
		SELECT from_state FROM shard_operations
		WHERE shard_id = ? AND to_state IN ('UPGRADING', 'SUSPENDED')
		ORDER BY ts DESC
		LIMIT 1
	`, id).Scan(&result).Error
	if err != nil {
		return "", err
	}
	return ShardState(result.FromState), nil
}
