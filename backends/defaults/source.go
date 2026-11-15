package defaults

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// SourceBackend is a source.SourceBackend over one SQLite file.
type SourceBackend struct {
	db        *sql.DB
	producers *sources.Registry
	now       func() time.Time
}

var _ source.Store = (*SourceBackend)(nil)

const sourceSchema = `
CREATE TABLE IF NOT EXISTS namespaces (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS sources (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	namespace_id INTEGER NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
	service TEXT NOT NULL,
	bucket_id INTEGER,
	kind TEXT NOT NULL,
	status TEXT NOT NULL,
	locator TEXT NOT NULL DEFAULT '',
	params TEXT NOT NULL DEFAULT '{}',
	schema_yaml TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	instance TEXT NOT NULL DEFAULT '',
	deployment TEXT NOT NULL DEFAULT '',
	sample TEXT NOT NULL DEFAULT '',
	detection TEXT NOT NULL DEFAULT '',
	sample_collected INTEGER NOT NULL DEFAULT 0,
	sample_updated_at TEXT,
	resample_requested_at TEXT,
	sample_size INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S', 'now') || '.000000000Z'),
	UNIQUE (namespace_id, service)
);
CREATE INDEX IF NOT EXISTS sources_locator ON sources (namespace_id, locator);
`

// SourceOption configures Open.
type SourceOption func(*SourceBackend)

// WithSourceProducers installs the registry of source kinds; the default is
// sources.Core.
func WithSourceProducers(r *sources.Registry) SourceOption {
	return func(s *SourceBackend) { s.producers = r }
}

// NewSourceBackend is a store over db, creating its tables if need be. db comes from
// sqlitedb.Open and is the caller's to close. Create this store before
// the token store: tokens refer to the namespaces table made here.
func NewSourceBackend(db *sql.DB, opts ...SourceOption) (*SourceBackend, error) {
	if _, err := db.Exec(sourceSchema); err != nil {
		return nil, fmt.Errorf("create sources schema: %w", err)
	}
	s := &SourceBackend{db: db, producers: sources.Core(nil), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Times are stored as fixed-width UTC text.
const sourceTimeFmt = "2006-01-02T15:04:05.000000000Z"

func sourceEnc(t time.Time) string { return t.UTC().Format(sourceTimeFmt) }

func (s *SourceBackend) nowText() string { return sourceEnc(s.now()) }

func sourceDecPtr(n sql.NullString) (*time.Time, error) {
	if !n.Valid {
		return nil, nil
	}
	t, err := time.Parse(sourceTimeFmt, n.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *SourceBackend) inTx(fn func(tx *sql.Tx) error) error {
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

func (s *SourceBackend) DeleteSource(namespaceID uint, service string) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE namespace_id = ? AND service = ?`, namespaceID, service)
	return err
}

// --- namespaces ---

func (s *SourceBackend) FindOrCreateNamespace(_ shape.Added, name string) (*source.Namespace, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO namespaces (name) VALUES (?)`, name); err != nil {
		return nil, err
	}
	ns, ok, err := s.GetNamespace(shape.Added{}, name)
	if err != nil || !ok {
		return nil, fmt.Errorf("namespace %q: %v", name, err)
	}
	return ns, nil
}

func (s *SourceBackend) GetNamespace(_ shape.Added, name string) (*source.Namespace, bool, error) {
	var ns source.Namespace
	err := s.db.QueryRow(`SELECT id, name FROM namespaces WHERE name = ?`, name).Scan(&ns.ID, &ns.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &ns, true, nil
}

func (s *SourceBackend) NamespaceName(_ shape.Added, id uint) (string, bool, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM namespaces WHERE id = ?`, id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

func (s *SourceBackend) ListNamespaces(shape.Added) ([]source.Namespace, error) {
	rows, err := s.db.Query(`SELECT id, name FROM namespaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Namespace
	for rows.Next() {
		var ns source.Namespace
		if err := rows.Scan(&ns.ID, &ns.Name); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

func (s *SourceBackend) DeleteNamespace(_ context.Context, _ shape.Added, name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow(`SELECT id FROM namespaces WHERE name = ?`, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return source.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM sources WHERE namespace_id = ?`, id); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM namespaces WHERE id = ?`, id)
		return err
	})
}

// --- sources ---

func sourceParamsJSON(p map[string]string) string {
	if len(p) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(p)
	return string(b)
}

const sourceCols = `s.id, s.namespace_id, n.name, s.service, s.bucket_id, s.kind, s.status, s.locator, s.params,
	s.schema_yaml, s.enabled, s.instance, s.deployment, s.sample, s.detection, s.sample_collected,
	s.sample_updated_at, s.resample_requested_at, s.sample_size, s.created_at`

type sourceScanner interface{ Scan(dest ...any) error }

func sourceScanSource(r sourceScanner) (*source.Source, error) {
	var (
		src                  source.Source
		bucket               sql.NullInt64
		params, status       string
		created              string
		enabled              int
		updatedAt, resampled sql.NullString
	)
	err := r.Scan(&src.ID, &src.NamespaceID, &src.Namespace, &src.Service, &bucket, &src.Kind, &status, &src.Locator, &params,
		&src.SchemaYAML, &enabled, &src.Instance, &src.Deployment, &src.Sample, &src.Detection, &src.SampleCollected,
		&updatedAt, &resampled, &src.SampleSize, &created)
	if err != nil {
		return nil, err
	}
	if src.CreatedAt, err = time.Parse(sourceTimeFmt, created); err != nil {
		return nil, err
	}
	src.Status = sources.Status(status)
	src.Enabled = enabled != 0
	if bucket.Valid {
		b := bucket.Int64
		src.BucketID = &b
	}
	if params != "" && params != "{}" {
		if err := json.Unmarshal([]byte(params), &src.Params); err != nil {
			return nil, fmt.Errorf("source %s/%s params: %w", src.Namespace, src.Service, err)
		}
	}
	if src.SampleUpdatedAt, err = sourceDecPtr(updatedAt); err != nil {
		return nil, err
	}
	if src.ResampleRequestedAt, err = sourceDecPtr(resampled); err != nil {
		return nil, err
	}
	return &src, nil
}

// sourceLoad is the source of (namespaceID, service) in tx, nil when absent.
func sourceLoad(tx *sql.Tx, namespaceID uint, service string) (*source.Source, error) {
	src, err := sourceScanSource(tx.QueryRow(`SELECT `+sourceCols+` FROM sources s JOIN namespaces n ON n.id = s.namespace_id
		WHERE s.namespace_id = ? AND s.service = ?`, namespaceID, service))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return src, err
}

func (s *SourceBackend) insert(tx *sql.Tx, namespaceID uint, kind, service string, status sources.Status, spec source.SourceSpec, schemaYAML string, resample bool) error {
	var resampled any
	if resample {
		resampled = s.nowText()
	}
	_, err := tx.Exec(`INSERT INTO sources (namespace_id, service, kind, status, locator, params, schema_yaml, resample_requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		namespaceID, service, kind, string(status), spec.Locator, sourceParamsJSON(spec.Params), schemaYAML, resampled)
	return err
}

func (s *SourceBackend) AddSource(_ context.Context, namespaceID uint, kind, service string, spec source.SourceSpec, schemaYAML string) error {
	return s.inTx(func(tx *sql.Tx) error {
		existing, err := sourceLoad(tx, namespaceID, service)
		if err != nil {
			return err
		}
		if existing == nil {
			return s.insert(tx, namespaceID, kind, service, sources.Configured, spec, schemaYAML, false)
		}
		_, err = tx.Exec(`UPDATE sources SET kind = ?, status = ?, locator = ?, params = ?, schema_yaml = ?, sample = '', detection = ?
			WHERE id = ?`, kind, string(sources.Configured), spec.Locator, sourceParamsJSON(spec.Params), schemaYAML, "", existing.ID)
		return err
	})
}

func (s *SourceBackend) RegisterSource(namespaceID uint, kind, service string, spec source.SourceSpec) error {
	return s.inTx(func(tx *sql.Tx) error {
		existing, err := sourceLoad(tx, namespaceID, service)
		if err != nil || existing != nil {
			return err
		}
		start := s.producers.Producer(kind).InitialState(sources.Registration{Locator: spec.Locator, Params: spec.Params})
		return s.insert(tx, namespaceID, kind, service, start.Status, spec, "", start.ResampleRequested)
	})
}

func (s *SourceBackend) EnsureSource(namespaceID uint, service string) error {
	return s.inTx(func(tx *sql.Tx) error {
		existing, err := sourceLoad(tx, namespaceID, service)
		if err != nil || existing != nil {
			return err
		}
		start := s.producers.Producer("direct").InitialState(sources.Registration{Locator: service})
		return s.insert(tx, namespaceID, "direct", service, start.Status, source.SourceSpec{Locator: service}, "", start.ResampleRequested)
	})
}

func (s *SourceBackend) ConfigureSource(_ context.Context, namespaceID uint, service, schemaYAML string) error {
	res, err := s.db.Exec(`UPDATE sources SET status = ?, schema_yaml = ?, sample = '', detection = '', sample_collected = 0,
		resample_requested_at = NULL
		WHERE namespace_id = ? AND service = ? AND status IN (?, ?)`,
		string(sources.Configured), schemaYAML, namespaceID, service, string(sources.Pending), string(sources.Configured))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return source.ErrNotFound
	}
	return nil
}

func (s *SourceBackend) FindSource(_ shape.Added, namespace, service string) (*source.Source, error) {
	src, err := sourceScanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources s JOIN namespaces n ON n.id = s.namespace_id
		WHERE n.name = ? AND s.service = ?`, namespace, service))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return src, err
}

func (s *SourceBackend) SourceSchema(_ context.Context, owner shape.Added, namespace, service string) (string, bool, error) {
	src, err := s.FindSource(owner, namespace, service)
	if err != nil || src == nil {
		return "", false, err
	}
	if src.Status != sources.Configured || src.SchemaYAML == "" {
		return "", false, nil
	}
	return src.SchemaYAML, true, nil
}

func (s *SourceBackend) FindServiceByLocator(_ shape.Added, namespace, locator string) (string, bool, error) {
	var service string
	err := s.db.QueryRow(`SELECT s.service FROM sources s JOIN namespaces n ON n.id = s.namespace_id
		WHERE n.name = ? AND s.locator = ?`, namespace, locator).Scan(&service)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return service, err == nil, err
}

// sourceAwaitingSchema is the predicate for sources a person still has to give a
// schema: those sampling or pending, plus configured ones collecting or holding
// a sample for review.
const sourceAwaitingSchema = `(s.status IN ('sampling', 'pending') OR (s.status = 'configured' AND (
		(s.schema_yaml = '' AND (s.sample_collected > 0 OR s.sample_updated_at IS NOT NULL))
		OR (s.schema_yaml <> '' AND (s.resample_requested_at IS NOT NULL OR s.sample NOT IN ('', '[]', 'null'))))))`

func (s *SourceBackend) ListSources(_ shape.Added, f source.Filter) ([]source.Source, error) {
	var where []string
	var args []any
	if f.Namespace != "" {
		where = append(where, "n.name = ?")
		args = append(args, f.Namespace)
	}
	if f.Namespaces != nil {
		if len(f.Namespaces) == 0 {
			return nil, nil
		}
		where = append(where, "n.name IN (?"+strings.Repeat(", ?", len(f.Namespaces)-1)+")")
		for _, n := range f.Namespaces {
			args = append(args, n)
		}
	}
	if f.AwaitingSchema {
		where = append(where, sourceAwaitingSchema)
	}
	q := `SELECT ` + sourceCols + ` FROM sources s JOIN namespaces n ON n.id = s.namespace_id`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q+` ORDER BY n.name, s.service`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Source
	for rows.Next() {
		src, err := sourceScanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

func (s *SourceBackend) SubmitSample(namespaceID uint, service string, sub source.SampleSubmission) error {
	return s.inTx(func(tx *sql.Tx) error {
		existing, err := sourceLoad(tx, namespaceID, service)
		if err != nil {
			return err
		}
		now := s.nowText()
		status := s.producers.Producer("weftd").InitialState(sources.Registration{Partial: sub.Partial}).Status
		if existing == nil {
			_, err := tx.Exec(`INSERT INTO sources (namespace_id, service, kind, status, locator, instance, deployment, sample, detection,
				sample_collected, sample_updated_at) VALUES (?, ?, 'weftd', ?, ?, ?, ?, ?, ?, ?, ?)`,
				namespaceID, service, string(status), service, sub.Instance, sub.Deployment, sub.SampleJSON, sub.DetectionJSON, sub.Collected, now)
			return err
		}
		if existing.Status == sources.Configured {
			if existing.ResampleRequestedAt == nil {
				return nil // a stale sample: the schema is approved
			}
			// A resample in flight: the fresh sample lands without touching the
			// status or the schema, and the final one ends the resample.
			status = sources.Configured
		}
		q := `UPDATE sources SET status = ?, instance = ?, deployment = ?, sample = ?, detection = ?, sample_collected = ?, sample_updated_at = ?`
		if !sub.Partial {
			q += `, resample_requested_at = NULL`
		}
		_, err = tx.Exec(q+` WHERE id = ?`, string(status), sub.Instance, sub.Deployment, sub.SampleJSON, sub.DetectionJSON, sub.Collected, now, existing.ID)
		return err
	})
}

func (s *SourceBackend) RequestResample(namespaceID uint, service string, size int) error {
	if size == 0 {
		size = source.SampleTarget
	}
	if size < source.SampleTarget || size > source.MaxSampleTarget {
		return source.ErrBadSampleSize
	}
	return s.inTx(func(tx *sql.Tx) error {
		src, err := sourceLoad(tx, namespaceID, service)
		if err != nil {
			return err
		}
		if src == nil {
			return source.ErrNotFound
		}
		if src.Status == sources.Sampling || (src.Status == sources.Configured && src.ResampleRequestedAt != nil) {
			return source.ErrAlreadySampling
		}
		plan := s.producers.Producer(src.Kind).Resample(src.View())
		now := s.nowText()
		q := `UPDATE sources SET resample_requested_at = ?, sample_size = ?`
		args := []any{now, size}
		if plan.Status != "" {
			q += `, status = ?`
			args = append(args, string(plan.Status))
		}
		if plan.ResetSample {
			q += `, sample = '', detection = '', sample_collected = 0, sample_updated_at = ?`
			args = append(args, now)
		}
		_, err = tx.Exec(q+` WHERE id = ?`, append(args, src.ID)...)
		return err
	})
}

func (s *SourceBackend) UpdateDetection(namespaceID uint, service, sampleJSON, detectionJSON string) error {
	_, err := s.db.Exec(`UPDATE sources SET detection = ? WHERE namespace_id = ? AND service = ? AND sample = ?`,
		detectionJSON, namespaceID, service, sampleJSON)
	return err
}

func (s *SourceBackend) ServicesInBucket(bucketID int64, unassigned bool) ([]shardstore.ShardService, error) {
	rows, err := s.db.Query(`SELECT namespace_id, service FROM sources WHERE bucket_id = ? OR (? AND bucket_id IS NULL)
		ORDER BY namespace_id, service`, bucketID, unassigned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shardstore.ShardService
	for rows.Next() {
		var sv shardstore.ShardService
		if err := rows.Scan(&sv.NamespaceID, &sv.Service); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}
