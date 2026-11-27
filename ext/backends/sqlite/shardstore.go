package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// ShardBackend is a shardstore.ShardBackend over one SQLite file.
type ShardBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ shardstore.Store = (*ShardBackend)(nil)

const shardSchema = `
CREATE TABLE IF NOT EXISTS buckets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	label TEXT NOT NULL UNIQUE,
	target_ingesting_count INTEGER NOT NULL,
	tier TEXT NOT NULL,
	created_at TEXT NOT NULL,
	window_duration TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'STASIS',
	cascade_started_at TEXT
);
CREATE TABLE IF NOT EXISTS shard_records (
	id TEXT PRIMARY KEY,
	window_start TEXT NOT NULL,
	window_end TEXT NOT NULL,
	state TEXT NOT NULL,
	ingest_failures INTEGER NOT NULL DEFAULT 0,
	addr TEXT NOT NULL DEFAULT '',
	instance_id TEXT NOT NULL DEFAULT '',
	parquet_path TEXT NOT NULL DEFAULT '',
	tier TEXT NOT NULL DEFAULT '',
	row_count INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	ebs_volume_id TEXT NOT NULL DEFAULT '',
	heartbeat_token TEXT NOT NULL DEFAULT '',
	bucket_id INTEGER NOT NULL REFERENCES buckets (id),
	last_heartbeat_at TEXT NOT NULL,
	az TEXT NOT NULL DEFAULT '',
	stripe_index INTEGER,
	launch_attempts INTEGER NOT NULL DEFAULT 0,
	binary_sha TEXT NOT NULL DEFAULT '',
	disk_bytes INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS shard_records_bucket ON shard_records (bucket_id, window_start);
CREATE TABLE IF NOT EXISTS shard_operations (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	shard_id TEXT NOT NULL,
	ts TEXT NOT NULL,
	from_state TEXT,
	to_state TEXT
);
CREATE INDEX IF NOT EXISTS shard_operations_shard ON shard_operations (shard_id, seq);
CREATE TABLE IF NOT EXISTS shard_services (
	shard_id TEXT NOT NULL,
	namespace_id INTEGER NOT NULL,
	service TEXT NOT NULL,
	PRIMARY KEY (shard_id, namespace_id, service)
);
CREATE TABLE IF NOT EXISTS shard_field_facts (
	shard_id TEXT NOT NULL,
	customer_id INTEGER NOT NULL,
	namespace TEXT NOT NULL,
	service TEXT NOT NULL,
	kind TEXT NOT NULL,
	env TEXT NOT NULL,
	locator TEXT NOT NULL,
	epoch INTEGER NOT NULL,
	PRIMARY KEY (shard_id, customer_id, namespace, service, kind, env, locator)
);
CREATE TABLE IF NOT EXISTS shard_heartbeats (
	shard_id TEXT NOT NULL,
	instance_id TEXT NOT NULL DEFAULT '',
	addr TEXT NOT NULL,
	state TEXT NOT NULL,
	row_count INTEGER NOT NULL DEFAULT 0,
	disk_bytes INTEGER NOT NULL DEFAULT 0,
	beat_at TEXT NOT NULL,
	PRIMARY KEY (shard_id, instance_id)
);
`

// NewShardBackend is a store over db, creating its tables if need be. db comes from
// sqlitedb.Open and is the caller's to close; other domains' stores may share it.
func NewShardBackend(db *sql.DB) (*ShardBackend, error) {
	if _, err := db.Exec(shardSchema + shardFactsSchema); err != nil {
		return nil, fmt.Errorf("create shard state schema: %w", err)
	}
	// The bucket services with no assignment belong to, as hosted's schema
	// seeds it. A deleted one comes back at the next start, and an operator's
	// edits to a surviving one stay.
	if _, err := db.Exec(`INSERT OR IGNORE INTO buckets (label, target_ingesting_count, tier, created_at, window_duration, state)
		VALUES ('default', 2, 'small', ?, '168h', 'STASIS')`, shardEnc(time.Now())); err != nil {
		return nil, fmt.Errorf("seed the default bucket: %w", err)
	}
	return &ShardBackend{db: db, now: time.Now}, nil
}

// Times are stored as fixed-width UTC text, so they sort as strings and the
// zero time round-trips.
const shardTimeFmt = "2006-01-02T15:04:05.000000000Z"

func shardEnc(t time.Time) string { return t.UTC().Format(shardTimeFmt) }

func shardEncPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return shardEnc(*t)
}

func shardDec(s string) (time.Time, error) { return time.Parse(shardTimeFmt, s) }

// inTx runs fn in a transaction.
func (s *ShardBackend) inTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// recordOp appends one state change to the history LatestPreRecoveryState and
// StuckArchiving read. A nil side is the row being created or deleted.
func (s *ShardBackend) recordOp(tx *sql.Tx, id string, from, to *shardstore.ShardState) error {
	str := func(v *shardstore.ShardState) any {
		if v == nil {
			return nil
		}
		return string(*v)
	}
	_, err := tx.Exec(`INSERT INTO shard_operations (shard_id, ts, from_state, to_state) VALUES (?, ?, ?, ?)`,
		id, shardEnc(s.now()), str(from), str(to))
	return err
}

// shardCurrentState is the shard's stored state, ok false when the row is absent.
func shardCurrentState(tx *sql.Tx, id string) (st shardstore.ShardState, ok bool, err error) {
	var v string
	err = tx.QueryRow(`SELECT state FROM shard_records WHERE id = ?`, id).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return shardstore.ShardState(v), err == nil, err
}

// --- shard rows ---

const shardCols = `id, window_start, window_end, state, ingest_failures, addr, instance_id, parquet_path,
	tier, row_count, created_at, ebs_volume_id, heartbeat_token, bucket_id, last_heartbeat_at, az,
	stripe_index, launch_attempts, binary_sha, disk_bytes`

type shardScanner interface{ Scan(dest ...any) error }

func shardScanShard(r shardScanner) (*shardstore.ShardRow, error) {
	var (
		sh                         shardstore.ShardRow
		ws, we, created, hb, state string
		stripe                     sql.NullInt64
	)
	err := r.Scan(&sh.ID, &ws, &we, &state, &sh.IngestFailures, &sh.Addr, &sh.InstanceID, &sh.ParquetPath,
		&sh.Tier, &sh.RowCount, &created, &sh.EBSVolumeID, &sh.HeartbeatToken, &sh.BucketID, &hb, &sh.AZ,
		&stripe, &sh.LaunchAttempts, &sh.BinarySHA, &sh.DiskBytes)
	if err != nil {
		return nil, err
	}
	sh.State = shardstore.ShardState(state)
	for _, f := range []struct {
		dst *time.Time
		src string
	}{{&sh.WindowStart, ws}, {&sh.WindowEnd, we}, {&sh.CreatedAt, created}, {&sh.LastHeartbeatAt, hb}} {
		if *f.dst, err = shardDec(f.src); err != nil {
			return nil, fmt.Errorf("shard %s: %w", sh.ID, err)
		}
	}
	if stripe.Valid {
		v := int(stripe.Int64)
		sh.StripeIndex = &v
	}
	return &sh, nil
}

func (s *ShardBackend) queryShards(where string, args ...any) ([]*shardstore.ShardRow, error) {
	rows, err := s.db.Query(`SELECT `+shardCols+` FROM shard_records `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*shardstore.ShardRow
	for rows.Next() {
		sh, err := shardScanShard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachFacts(out)
}

func (s *ShardBackend) SaveShard(sh *shardstore.ShardRow) error {
	return s.inTx(func(tx *sql.Tx) error {
		old, existed, err := shardCurrentState(tx, sh.ID)
		if err != nil {
			return err
		}
		var stripe any
		if sh.StripeIndex != nil {
			stripe = *sh.StripeIndex
		}
		_, err = tx.Exec(`INSERT INTO shard_records (`+shardCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				window_start = excluded.window_start, window_end = excluded.window_end, state = excluded.state,
				ingest_failures = excluded.ingest_failures, addr = excluded.addr, instance_id = excluded.instance_id,
				parquet_path = excluded.parquet_path, tier = excluded.tier, row_count = excluded.row_count,
				created_at = excluded.created_at, ebs_volume_id = excluded.ebs_volume_id,
				heartbeat_token = excluded.heartbeat_token, bucket_id = excluded.bucket_id,
				last_heartbeat_at = excluded.last_heartbeat_at, az = excluded.az, stripe_index = excluded.stripe_index,
				launch_attempts = excluded.launch_attempts, binary_sha = excluded.binary_sha,
				disk_bytes = excluded.disk_bytes`,
			sh.ID, shardEnc(sh.WindowStart), shardEnc(sh.WindowEnd), string(sh.State), sh.IngestFailures, sh.Addr,
			sh.InstanceID, sh.ParquetPath, sh.Tier, sh.RowCount, shardEnc(sh.CreatedAt), sh.EBSVolumeID,
			sh.HeartbeatToken, sh.BucketID, shardEnc(sh.LastHeartbeatAt), sh.AZ, stripe, sh.LaunchAttempts,
			sh.BinarySHA, sh.DiskBytes)
		if err != nil {
			return err
		}
		if err := shardSaveFacts(tx, sh); err != nil {
			return err
		}
		switch {
		case !existed:
			return s.recordOp(tx, sh.ID, nil, &sh.State)
		case old != sh.State:
			return s.recordOp(tx, sh.ID, &old, &sh.State)
		}
		return nil
	})
}

func (s *ShardBackend) GetShard(id string) (*shardstore.ShardRow, bool, error) {
	sh, err := shardScanShard(s.db.QueryRow(`SELECT `+shardCols+` FROM shard_records WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := s.attachFacts([]*shardstore.ShardRow{sh}); err != nil {
		return nil, false, err
	}
	return sh, true, nil
}

func (s *ShardBackend) ListShards() ([]*shardstore.ShardRow, error) { return s.queryShards(``) }

func (s *ShardBackend) ListShardsByBucket(bucketID int64) ([]*shardstore.ShardRow, error) {
	return s.queryShards(`WHERE bucket_id = ? ORDER BY window_start DESC`, bucketID)
}

func (s *ShardBackend) DeleteShard(id string) error {
	return s.inTx(func(tx *sql.Tx) error {
		old, existed, err := shardCurrentState(tx, id)
		if err != nil || !existed {
			return err
		}
		for _, t := range []string{"shard_services", "shard_field_facts", "shard_export_facts", "shard_upgrades"} {
			if _, err := tx.Exec(`DELETE FROM `+t+` WHERE shard_id = ?`, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM shard_records WHERE id = ?`, id); err != nil {
			return err
		}
		return s.recordOp(tx, id, &old, nil)
	})
}

// exec runs one narrow write on a shard row. A missing row is not an error.
func (s *ShardBackend) exec(q string, args ...any) error {
	_, err := s.db.Exec(q, args...)
	return err
}

func (s *ShardBackend) SetShardState(id string, state shardstore.ShardState) error {
	return s.inTx(func(tx *sql.Tx) error {
		old, existed, err := shardCurrentState(tx, id)
		if err != nil || !existed {
			return err
		}
		if _, err := tx.Exec(`UPDATE shard_records SET state = ? WHERE id = ?`, string(state), id); err != nil {
			return err
		}
		if old == state {
			return nil
		}
		return s.recordOp(tx, id, &old, &state)
	})
}

func (s *ShardBackend) TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error {
	if diskBytes > 0 {
		return s.exec(`UPDATE shard_records SET row_count = ?, last_heartbeat_at = ?, binary_sha = ?, disk_bytes = ? WHERE id = ?`,
			rowCount, shardEnc(at), binarySHA, diskBytes, id)
	}
	return s.exec(`UPDATE shard_records SET row_count = ?, last_heartbeat_at = ?, binary_sha = ? WHERE id = ?`,
		rowCount, shardEnc(at), binarySHA, id)
}

func (s *ShardBackend) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	var stripe any
	if stripeIndex != nil {
		stripe = *stripeIndex
	}
	return s.exec(`UPDATE shard_records SET instance_id = ?, az = ?, ebs_volume_id = ?, stripe_index = ? WHERE id = ?`,
		instanceID, az, volumeID, stripe, id)
}

func (s *ShardBackend) SetShardAddr(id, instanceID, addr string) error {
	res, err := s.db.Exec(`UPDATE shard_records SET addr = ?
		WHERE id = ? AND (? = '' OR instance_id = ? OR instance_id = '')`, addr, id, instanceID, instanceID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return shardstore.ErrShardMoved
	}
	return nil
}

func (s *ShardBackend) SetShardIngestFailures(id string, n int) error {
	return s.exec(`UPDATE shard_records SET ingest_failures = ? WHERE id = ?`, n, id)
}

func (s *ShardBackend) IncrementShardLaunchAttempts(id string) error {
	return s.exec(`UPDATE shard_records SET launch_attempts = launch_attempts + 1 WHERE id = ?`, id)
}

func (s *ShardBackend) SetShardParquetPath(id, path string) error {
	return s.exec(`UPDATE shard_records SET parquet_path = ? WHERE id = ?`, path, id)
}

func (s *ShardBackend) ClearShardVolume(id string) error {
	return s.exec(`UPDATE shard_records SET ebs_volume_id = '' WHERE id = ?`, id)
}

func (s *ShardBackend) ClearShardInstance(id string) error {
	return s.exec(`UPDATE shard_records SET instance_id = '' WHERE id = ?`, id)
}

func (s *ShardBackend) LatestPreRecoveryState(id string) (shardstore.ShardState, error) {
	var from sql.NullString
	err := s.db.QueryRow(`SELECT from_state FROM shard_operations
		WHERE shard_id = ? AND to_state IN ('UPGRADING', 'SUSPENDED') ORDER BY seq DESC LIMIT 1`, id).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return shardstore.ShardState(from.String), err
}

// StuckArchiving reports each ARCHIVING shard's latest entry into ARCHIVING.
// The open build keeps no export attempts, so Attempts is always zero.
func (s *ShardBackend) StuckArchiving(before time.Time) ([]shardstore.StuckShard, error) {
	rows, err := s.db.Query(`
		SELECT r.id, MAX(o.ts) AS since
		FROM shard_records r
		JOIN shard_operations o ON o.shard_id = r.id AND o.to_state = 'ARCHIVING'
		WHERE r.state = 'ARCHIVING'
		GROUP BY r.id
		HAVING MAX(o.ts) < ?
		ORDER BY since`, shardEnc(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.StuckShard
	for rows.Next() {
		var id, since string
		if err := rows.Scan(&id, &since); err != nil {
			return nil, err
		}
		t, err := shardDec(since)
		if err != nil {
			return nil, err
		}
		out = append(out, shardstore.StuckShard{ShardID: id, Since: t})
	}
	return out, rows.Err()
}

// --- shard services and field facts ---

func (s *ShardBackend) StampShardServices(shardID string, services []shardstore.ShardService) error {
	return s.inTx(func(tx *sql.Tx) error {
		for _, sv := range services {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO shard_services (shard_id, namespace_id, service) VALUES (?, ?, ?)`,
				shardID, sv.NamespaceID, sv.Service); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *ShardBackend) AddShardService(shardID string, namespaceID uint, service string) error {
	return s.exec(`INSERT OR IGNORE INTO shard_services (shard_id, namespace_id, service) VALUES (?, ?, ?)`,
		shardID, namespaceID, service)
}

func (s *ShardBackend) ListShardServices() ([]shardstore.ShardService, error) {
	rows, err := s.db.Query(`SELECT shard_id, namespace_id, service FROM shard_services`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.ShardService
	for rows.Next() {
		var sv shardstore.ShardService
		if err := rows.Scan(&sv.ShardID, &sv.NamespaceID, &sv.Service); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

func (s *ShardBackend) ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error) {
	q, args := `SELECT DISTINCT shard_id FROM shard_services WHERE namespace_id = ?`, []any{namespaceID}
	if service != "" {
		q, args = q+` AND service = ?`, append(args, service)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func (s *ShardBackend) ListShardFieldFacts() ([]shardstore.ShardFieldFact, error) {
	rows, err := s.db.Query(`SELECT shard_id, customer_id, namespace, service, kind, env, locator, epoch FROM shard_field_facts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.ShardFieldFact
	for rows.Next() {
		var f shardstore.ShardFieldFact
		if err := rows.Scan(&f.ShardID, &f.CustomerID, &f.Namespace, &f.Service, &f.Kind, &f.Env, &f.Locator, &f.Epoch); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *ShardBackend) ApplyShardFieldFacts(shardID string, typed, dropped []shardstore.ShardFieldFact) error {
	return s.inTx(func(tx *sql.Tx) error {
		for _, f := range typed {
			if _, err := tx.Exec(`INSERT INTO shard_field_facts (shard_id, customer_id, namespace, service, kind, env, locator, epoch)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (shard_id, customer_id, namespace, service, kind, env, locator) DO UPDATE SET epoch = excluded.epoch`,
				shardID, f.CustomerID, f.Namespace, f.Service, f.Kind, f.Env, f.Locator, f.Epoch); err != nil {
				return err
			}
		}
		for _, f := range dropped {
			if _, err := tx.Exec(`DELETE FROM shard_field_facts
				WHERE shard_id = ? AND customer_id = ? AND namespace = ? AND service = ? AND kind = ? AND env = ? AND locator = ?`,
				shardID, f.CustomerID, f.Namespace, f.Service, f.Kind, f.Env, f.Locator); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- heartbeats ---

func (s *ShardBackend) RecordHeartbeat(hb *shardstore.ShardHeartbeat) error {
	return s.exec(`INSERT INTO shard_heartbeats (shard_id, instance_id, addr, state, row_count, disk_bytes, beat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (shard_id, instance_id) DO UPDATE SET addr = excluded.addr, state = excluded.state,
			row_count = excluded.row_count, disk_bytes = excluded.disk_bytes, beat_at = excluded.beat_at`,
		hb.ShardID, hb.InstanceID, hb.Addr, hb.State, hb.RowCount, hb.DiskBytes, shardEnc(s.now()))
}

func (s *ShardBackend) LiveShardHeartbeats(within time.Duration) ([]shardstore.ShardHeartbeat, error) {
	now := s.now()
	rows, err := s.db.Query(`SELECT shard_id, instance_id, addr, state, row_count, disk_bytes, beat_at
		FROM shard_heartbeats WHERE beat_at > ? ORDER BY beat_at DESC`, shardEnc(now.Add(-within)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.ShardHeartbeat
	claimed := map[string]bool{}
	for rows.Next() {
		var h shardstore.ShardHeartbeat
		var beat string
		if err := rows.Scan(&h.ShardID, &h.InstanceID, &h.Addr, &h.State, &h.RowCount, &h.DiskBytes, &beat); err != nil {
			return nil, err
		}
		// Newest first: a claimed shard keeps its newest beat only, so a
		// replaced instance's old row cannot resurface its dead address.
		if h.ShardID != "" {
			if claimed[h.ShardID] {
				continue
			}
			claimed[h.ShardID] = true
		}
		t, err := shardDec(beat)
		if err != nil {
			return nil, err
		}
		h.Age = now.Sub(t)
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *ShardBackend) PruneShardHeartbeats(maxAge time.Duration) error {
	return s.exec(`DELETE FROM shard_heartbeats WHERE beat_at < ?`, shardEnc(s.now().Add(-maxAge)))
}

// --- buckets ---

const shardBucketCols = `id, label, target_ingesting_count, tier, created_at, window_duration, state, cascade_started_at`

func shardScanBucket(r shardScanner) (shardstore.Bucket, error) {
	var b shardstore.Bucket
	var cascade sql.NullString
	if err := r.Scan(&b.ID, &b.Label, &b.TargetIngestingCount, &b.Tier, &b.CreatedAt, &b.WindowDuration, &b.State, &cascade); err != nil {
		return b, err
	}
	if cascade.Valid {
		t, err := shardDec(cascade.String)
		if err != nil {
			return b, err
		}
		b.CascadeStartedAt = &t
	}
	return b, nil
}

func (s *ShardBackend) ListBuckets() ([]shardstore.Bucket, error) {
	rows, err := s.db.Query(`SELECT ` + shardBucketCols + ` FROM buckets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.Bucket
	for rows.Next() {
		b, err := shardScanBucket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *ShardBackend) getBucket(where string, arg any) (shardstore.Bucket, bool, error) {
	b, err := shardScanBucket(s.db.QueryRow(`SELECT `+shardBucketCols+` FROM buckets WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return shardstore.Bucket{}, false, nil
	}
	return b, err == nil, err
}

func (s *ShardBackend) GetBucket(id int64) (shardstore.Bucket, bool, error) {
	return s.getBucket(`id = ?`, id)
}

func (s *ShardBackend) GetBucketByLabel(label string) (shardstore.Bucket, bool, error) {
	return s.getBucket(`label = ?`, label)
}

// SaveBucket inserts a bucket with no id, assigning one back, and otherwise
// updates it in place.
func (s *ShardBackend) SaveBucket(b *shardstore.Bucket) error {
	state := b.State
	if state == "" {
		state = string(shardstore.BucketStasis)
	}
	if b.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO buckets (label, target_ingesting_count, tier, created_at, window_duration, state, cascade_started_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			b.Label, b.TargetIngestingCount, b.Tier, b.CreatedAt, b.WindowDuration, state, shardEncPtr(b.CascadeStartedAt))
		if err != nil {
			return err
		}
		b.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`INSERT INTO buckets (`+shardBucketCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET label = excluded.label, target_ingesting_count = excluded.target_ingesting_count,
			tier = excluded.tier, created_at = excluded.created_at, window_duration = excluded.window_duration,
			state = excluded.state, cascade_started_at = excluded.cascade_started_at`,
		b.ID, b.Label, b.TargetIngestingCount, b.Tier, b.CreatedAt, b.WindowDuration, state, shardEncPtr(b.CascadeStartedAt))
	return err
}

func (s *ShardBackend) BucketState(id int64) (shardstore.BucketState, error) {
	var st string
	err := s.db.QueryRow(`SELECT state FROM buckets WHERE id = ?`, id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", shardstore.ErrUnknownBucket
	}
	if err != nil {
		return "", fmt.Errorf("bucketsm read %d: %w", id, err)
	}
	if st == "" {
		return shardstore.BucketStasis, nil
	}
	return shardstore.BucketState(st), nil
}

func (s *ShardBackend) SetBucketState(id int64, st shardstore.BucketState) error {
	return s.exec(`UPDATE buckets SET state = ? WHERE id = ?`, string(st), id)
}

func (s *ShardBackend) SetBucketCascadeStart(id int64, at *time.Time) error {
	return s.exec(`UPDATE buckets SET cascade_started_at = ? WHERE id = ?`, shardEncPtr(at), id)
}

func (s *ShardBackend) ShardsInBucketStates(bucketID int64, states ...shardstore.ShardState) ([]*shardstore.ShardRow, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := []any{bucketID}
	marks := ""
	for i, st := range states {
		if i > 0 {
			marks += ", "
		}
		marks += "?"
		args = append(args, string(st))
	}
	return s.queryShards(`WHERE bucket_id = ? AND state IN (`+marks+`)`, args...)
}

// The export and upgrade facts of a shard live in their own tables, beside the
// shard row: SaveShard writes them in its transaction and every read that
// returns shard rows attaches them.
const shardFactsSchema = `
CREATE TABLE IF NOT EXISTS shard_export_facts (
	shard_id TEXT PRIMARY KEY,
	snapshot_id TEXT NOT NULL DEFAULT '',
	instance_id TEXT NOT NULL DEFAULT '',
	instance_type TEXT NOT NULL DEFAULT '',
	started_at TEXT,
	attempt INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS shard_upgrades (
	shard_id TEXT PRIMARY KEY,
	phase TEXT NOT NULL DEFAULT '',
	origin TEXT NOT NULL DEFAULT '',
	phase_at TEXT,
	paused_at TEXT,
	successor_instance_id TEXT NOT NULL DEFAULT '',
	successor_addr TEXT NOT NULL DEFAULT '',
	demoted_instance_id TEXT NOT NULL DEFAULT '',
	demoted_addr TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	parent_span_id TEXT NOT NULL DEFAULT ''
);
`

func shardDecPtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := shardDec(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// attachFacts loads the export and upgrade facts of rows onto them.
func (s *ShardBackend) attachFacts(rows []*shardstore.ShardRow) error {
	if len(rows) == 0 {
		return nil
	}
	byID := make(map[string]*shardstore.ShardRow, len(rows))
	args := make([]any, len(rows))
	for i, r := range rows {
		byID[r.ID] = r
		args[i] = r.ID
	}
	in := `(` + strings.TrimSuffix(strings.Repeat("?,", len(rows)), ",") + `)`

	ups, err := s.db.Query(`SELECT shard_id, phase, origin, phase_at, paused_at, successor_instance_id, successor_addr,
		demoted_instance_id, demoted_addr, trace_id, parent_span_id FROM shard_upgrades WHERE shard_id IN `+in, args...)
	if err != nil {
		return err
	}
	defer ups.Close()
	for ups.Next() {
		var (
			id, phase, origin string
			at, paused        sql.NullString
			f                 shardstore.UpgradeFacts
		)
		if err := ups.Scan(&id, &phase, &origin, &at, &paused, &f.SuccessorInstanceID, &f.SuccessorAddr,
			&f.DemotedInstanceID, &f.DemotedAddr, &f.TraceID, &f.ParentSpanID); err != nil {
			return err
		}
		f.Phase, f.Origin = shardstore.UpgradePhase(phase), shardstore.ShardState(origin)
		if f.PhaseAt, err = shardDecPtr(at); err != nil {
			return err
		}
		if f.PausedAt, err = shardDecPtr(paused); err != nil {
			return err
		}
		byID[id].Upgrade = f
	}
	if err := ups.Err(); err != nil {
		return err
	}

	exps, err := s.db.Query(`SELECT shard_id, snapshot_id, instance_id, instance_type, started_at, attempt
		FROM shard_export_facts WHERE shard_id IN `+in, args...)
	if err != nil {
		return err
	}
	defer exps.Close()
	for exps.Next() {
		var (
			id      string
			started sql.NullString
			f       shardstore.ExportFacts
		)
		if err := exps.Scan(&id, &f.SnapshotID, &f.Instance, &f.InstanceType, &started, &f.Attempt); err != nil {
			return err
		}
		if f.StartedAt, err = shardDecPtr(started); err != nil {
			return err
		}
		byID[id].Export = f
	}
	return exps.Err()
}

// shardSaveFacts writes sh's facts in tx, after its row exists. A zero upgrade
// removes the row; the export row stays once an attempt was counted, since the
// counter outlives the attempt's other facts.
func shardSaveFacts(tx *sql.Tx, sh *shardstore.ShardRow) error {
	if sh.Upgrade == (shardstore.UpgradeFacts{}) {
		if _, err := tx.Exec(`DELETE FROM shard_upgrades WHERE shard_id = ?`, sh.ID); err != nil {
			return err
		}
	} else if err := shardPutUpgrade(tx, sh.ID, sh.Upgrade); err != nil {
		return err
	}
	if sh.Export == (shardstore.ExportFacts{}) {
		_, err := tx.Exec(`DELETE FROM shard_export_facts WHERE shard_id = ?`, sh.ID)
		return err
	}
	f := sh.Export
	_, err := tx.Exec(`INSERT INTO shard_export_facts (shard_id, snapshot_id, instance_id, instance_type, started_at, attempt)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (shard_id) DO UPDATE SET snapshot_id = excluded.snapshot_id, instance_id = excluded.instance_id,
			instance_type = excluded.instance_type, started_at = excluded.started_at, attempt = excluded.attempt`,
		sh.ID, f.SnapshotID, f.Instance, f.InstanceType, shardEncPtr(f.StartedAt), f.Attempt)
	return err
}

func shardPutUpgrade(tx *sql.Tx, id string, f shardstore.UpgradeFacts) error {
	_, err := tx.Exec(`INSERT INTO shard_upgrades (shard_id, phase, origin, phase_at, paused_at, successor_instance_id,
			successor_addr, demoted_instance_id, demoted_addr, trace_id, parent_span_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (shard_id) DO UPDATE SET phase = excluded.phase, origin = excluded.origin,
			phase_at = excluded.phase_at, paused_at = excluded.paused_at,
			successor_instance_id = excluded.successor_instance_id, successor_addr = excluded.successor_addr,
			demoted_instance_id = excluded.demoted_instance_id, demoted_addr = excluded.demoted_addr,
			trace_id = excluded.trace_id, parent_span_id = excluded.parent_span_id`,
		id, string(f.Phase), string(f.Origin), shardEncPtr(f.PhaseAt), shardEncPtr(f.PausedAt), f.SuccessorInstanceID,
		f.SuccessorAddr, f.DemotedInstanceID, f.DemotedAddr, f.TraceID, f.ParentSpanID)
	return err
}

// BeginExportAttempt claims the next export attempt. SQLite serializes writers,
// so the transaction alone makes the claim and the cap exact.
func (s *ShardBackend) BeginExportAttempt(id string, prevAttempt int, instanceType string, startedAt time.Time, maxActive int) (shardstore.ExportClaim, error) {
	claim := shardstore.ExportClaimLost
	err := s.inTx(func(tx *sql.Tx) error {
		if _, ok, err := shardCurrentState(tx, id); err != nil || !ok {
			return err // lost: the row is gone
		}
		have := 0
		err := tx.QueryRow(`SELECT attempt FROM shard_export_facts WHERE shard_id = ?`, id).Scan(&have)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if have != prevAttempt {
			return nil // lost: someone else already claimed
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM shard_export_facts WHERE started_at IS NOT NULL AND shard_id <> ?`, id).Scan(&active); err != nil {
			return err
		}
		if active >= maxActive {
			claim = shardstore.ExportClaimHeld
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO shard_export_facts (shard_id, instance_type, started_at, attempt) VALUES (?, ?, ?, ?)
			ON CONFLICT (shard_id) DO UPDATE SET instance_type = excluded.instance_type,
				started_at = excluded.started_at, attempt = excluded.attempt`,
			id, instanceType, shardEnc(startedAt), prevAttempt+1); err != nil {
			return err
		}
		claim = shardstore.ExportClaimWon
		return nil
	})
	return claim, err
}

func (s *ShardBackend) SetExportSnapshot(id, snapshotID string) error {
	return s.exec(`INSERT INTO shard_export_facts (shard_id, snapshot_id) VALUES (?, ?)
		ON CONFLICT (shard_id) DO UPDATE SET snapshot_id = excluded.snapshot_id`, id, snapshotID)
}

func (s *ShardBackend) SetExportInstance(id, instanceID string) error {
	return s.exec(`INSERT INTO shard_export_facts (shard_id, instance_id) VALUES (?, ?)
		ON CONFLICT (shard_id) DO UPDATE SET instance_id = excluded.instance_id`, id, instanceID)
}

// ClearExportFacts ends the attempt's bookkeeping but keeps the counter.
func (s *ShardBackend) ClearExportFacts(id string) error {
	return s.exec(`UPDATE shard_export_facts SET snapshot_id = '', instance_id = '', instance_type = '', started_at = NULL
		WHERE shard_id = ?`, id)
}

// LatestShardDiskBytes is the shard's last reported disk footprint, 0 when it
// never reported one or the row is gone.
func (s *ShardBackend) LatestShardDiskBytes(id string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT disk_bytes FROM shard_records WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// SetShardUpgrade is a compare-and-set of the upgrade facts on the phase the
// writer last saw.
func (s *ShardBackend) SetShardUpgrade(id string, from shardstore.UpgradePhase, f shardstore.UpgradeFacts) error {
	moved := fmt.Errorf("%w: %s is no longer in phase %q", shardstore.ErrUpgradeMoved, id, from)
	return s.inTx(func(tx *sql.Tx) error {
		if _, ok, err := shardCurrentState(tx, id); err != nil {
			return err
		} else if !ok {
			return moved
		}
		var have string
		err := tx.QueryRow(`SELECT phase FROM shard_upgrades WHERE shard_id = ?`, id).Scan(&have)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if from == "" {
			if f == (shardstore.UpgradeFacts{}) {
				return nil
			}
			if exists {
				return moved
			}
			return shardPutUpgrade(tx, id, f)
		}
		if !exists || have != string(from) {
			return moved
		}
		if f == (shardstore.UpgradeFacts{}) {
			_, err := tx.Exec(`DELETE FROM shard_upgrades WHERE shard_id = ?`, id)
			return err
		}
		return shardPutUpgrade(tx, id, f)
	})
}
