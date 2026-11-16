package defaults

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/shape"
)

// CronBackend is a cron.CronBackend over the install's SQLite database.
type CronBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ cron.Store = (*CronBackend)(nil)

const cronSchema = `
CREATE TABLE IF NOT EXISTS cron_watches (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	namespace_id INTEGER REFERENCES namespaces (id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	schedule TEXT NOT NULL,
	pipeline TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	consecutive_strikes INTEGER NOT NULL DEFAULT 0,
	last_run_at TEXT,
	last_status TEXT NOT NULL DEFAULT '',
	value_field TEXT,
	tag_field TEXT,
	has_alert INTEGER NOT NULL DEFAULT 0,
	value_type TEXT NOT NULL DEFAULT 'number',
	alert_condition TEXT,
	alert_threshold REAL,
	content_hash TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	authored_by TEXT NOT NULL DEFAULT '',
	lowered_plan BLOB,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	deleted_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS cron_watches_home_name ON cron_watches (COALESCE(namespace_id, 0), name);
CREATE TABLE IF NOT EXISTS cron_results (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	cron_watch_id INTEGER NOT NULL REFERENCES cron_watches (id) ON DELETE CASCADE,
	ran_at TEXT NOT NULL,
	stdout TEXT NOT NULL DEFAULT '',
	stderr TEXT NOT NULL DEFAULT '',
	exit_code INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	value REAL,
	tag TEXT NOT NULL DEFAULT '',
	text_value TEXT
);
CREATE INDEX IF NOT EXISTS cron_results_watch_ran ON cron_results (cron_watch_id, ran_at);
CREATE TABLE IF NOT EXISTS alert_incidents (
	id TEXT PRIMARY KEY,
	cron_watch_id INTEGER NOT NULL REFERENCES cron_watches (id) ON DELETE CASCADE,
	tag TEXT NOT NULL DEFAULT '',
	opened_at TEXT NOT NULL,
	closed_at TEXT,
	last_seen_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS alert_incidents_open ON alert_incidents (cron_watch_id, tag) WHERE closed_at IS NULL;
CREATE TABLE IF NOT EXISTS alert_incident_deliveries (
	incident_id TEXT NOT NULL REFERENCES alert_incidents (id) ON DELETE CASCADE,
	target TEXT NOT NULL,
	delivered_at TEXT NOT NULL,
	PRIMARY KEY (incident_id, target)
);
`

// NewCronBackend is a store over db, creating its tables if need be. db comes from
// sqlitedb.Open and is the caller's to close. The namespaces table must already
// exist: create the source store on the same db first.
func NewCronBackend(db *sql.DB) (*CronBackend, error) {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'namespaces'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("create crons store: no namespaces table; create the sources store on this database first")
	}
	if _, err := db.Exec(cronSchema); err != nil {
		return nil, fmt.Errorf("create crons schema: %w", err)
	}
	return &CronBackend{db: db, now: time.Now}, nil
}

// Times are stored as fixed-width UTC text, so they sort as strings.
const cronTimeFmt = "2006-01-02T15:04:05.000000000Z"

func cronEnc(t time.Time) string { return t.UTC().Format(cronTimeFmt) }

func cronDec(s string) (time.Time, error) { return time.Parse(cronTimeFmt, s) }

func cronDecPtr(n sql.NullString) (*time.Time, error) {
	if !n.Valid {
		return nil, nil
	}
	t, err := cronDec(n.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *CronBackend) inTx(fn func(tx *sql.Tx) error) error {
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

// cronHome is the namespace_id column value for a cronHome: 0 is the owner level.
func cronHome(ns uint) any {
	if ns == 0 {
		return nil
	}
	return ns
}

// cronInHome is the SQL condition that a row is in a home; the argument is home(ns).
const cronInHome = `COALESCE(namespace_id, 0) = COALESCE(?, 0)`

const cronWatchCols = `id, namespace_id, name, schedule, pipeline, enabled, consecutive_strikes, last_run_at, last_status,
	value_field, tag_field, has_alert, value_type, alert_condition, alert_threshold, content_hash, message, created_by, authored_by,
	lowered_plan, created_at, updated_at`

type cronScanner interface{ Scan(dest ...any) error }

func cronScanWatch(r cronScanner) (*cron.Watch, error) {
	var (
		w                    cron.Watch
		ns                   sql.NullInt64
		enabled, hasAlert    int
		lastRun              sql.NullString
		valueField, tagField sql.NullString
		condition            sql.NullString
		threshold            sql.NullFloat64
		created, updated     string
	)
	if err := r.Scan(&w.ID, &ns, &w.Name, &w.Schedule, &w.Pipeline, &enabled, &w.ConsecutiveStrikes, &lastRun, &w.LastStatus,
		&valueField, &tagField, &hasAlert, &w.Compiled.ValueType, &condition, &threshold, &w.ContentHash, &w.Message, &w.CreatedBy, &w.AuthoredBy,
		&w.LoweredPlan, &created, &updated); err != nil {
		return nil, err
	}
	w.Namespace = uint(ns.Int64)
	w.Enabled, w.Compiled.HasAlert = enabled != 0, hasAlert != 0
	if valueField.Valid {
		w.Compiled.ValueField = &valueField.String
	}
	if tagField.Valid {
		w.Compiled.TagField = &tagField.String
	}
	if condition.Valid {
		w.Compiled.AlertCondition = &condition.String
	}
	if threshold.Valid {
		w.Compiled.AlertThreshold = &threshold.Float64
	}
	var err error
	if w.LastRunAt, err = cronDecPtr(lastRun); err != nil {
		return nil, err
	}
	if w.CreatedAt, err = cronDec(created); err != nil {
		return nil, err
	}
	if w.UpdatedAt, err = cronDec(updated); err != nil {
		return nil, err
	}
	w.WatchSpec.Schedule, w.WatchSpec.Pipeline, w.WatchSpec.Enabled = w.Schedule, w.Pipeline, w.Enabled
	w.WatchSpec.LoweredPlan = w.LoweredPlan
	return &w, nil
}

func cronB2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func cronNullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func cronNullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func cronPlanArg(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	return p
}

func (s *CronBackend) SaveWatch(_ shape.Added, ns uint, author, name string, w cron.WatchSpec) error {
	hash := cron.ContentHash(w.Schedule, w.Pipeline)
	now := cronEnc(s.now())
	c := w.Compiled
	vt := c.ValueType
	if vt == "" {
		vt = cron.ValueNumber
	}
	return s.inTx(func(tx *sql.Tx) error {
		var (
			id          int64
			oldHash     string
			oldEnabled  int
			deleted     sql.NullString
			found       = true
			existingErr = tx.QueryRow(`SELECT id, content_hash, enabled, deleted_at FROM cron_watches WHERE `+cronInHome+` AND name = ?`,
				cronHome(ns), name).Scan(&id, &oldHash, &oldEnabled, &deleted)
		)
		if errors.Is(existingErr, sql.ErrNoRows) {
			found = false
		} else if existingErr != nil {
			return existingErr
		}
		if !found {
			_, err := tx.Exec(`INSERT INTO cron_watches (namespace_id, name, schedule, pipeline, enabled, value_field, tag_field, has_alert,
				value_type, alert_condition, alert_threshold, content_hash, created_by, authored_by, lowered_plan, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				cronHome(ns), name, w.Schedule, w.Pipeline, cronB2i(w.Enabled), cronNullStr(c.ValueField), cronNullStr(c.TagField), cronB2i(c.HasAlert),
				vt, cronNullStr(c.AlertCondition), cronNullFloat(c.AlertThreshold), hash, author, author, cronPlanArg(w.LoweredPlan), now, now)
			return err
		}
		// A save that changes nothing is a no-op.
		if oldHash == hash && !deleted.Valid && (oldEnabled != 0) == w.Enabled {
			return nil
		}
		// An overwrite keeps the original creator, sets who wrote the version and
		// clears its message, and turning a benched watch back on wipes its strikes.
		strikes := ""
		if w.Enabled && oldEnabled == 0 {
			strikes = ", consecutive_strikes = 0"
		}
		_, err := tx.Exec(`UPDATE cron_watches SET deleted_at = NULL, schedule = ?, pipeline = ?, content_hash = ?, value_field = ?,
			tag_field = ?, has_alert = ?, value_type = ?, alert_condition = ?, alert_threshold = ?, lowered_plan = ?, enabled = ?,
			message = '', authored_by = ?, updated_at = ?`+strikes+` WHERE id = ?`,
			w.Schedule, w.Pipeline, hash, cronNullStr(c.ValueField), cronNullStr(c.TagField), cronB2i(c.HasAlert), vt,
			cronNullStr(c.AlertCondition), cronNullFloat(c.AlertThreshold), cronPlanArg(w.LoweredPlan), cronB2i(w.Enabled), author, now, id)
		return err
	})
}

func (s *CronBackend) AmendMessage(_ shape.Added, ns uint, authorID, name, message string) error {
	var author string
	err := s.db.QueryRow(`SELECT authored_by FROM cron_watches WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`, cronHome(ns), name).Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return cron.ErrNotFound
	}
	if err != nil {
		return err
	}
	if authorID != "" && author != authorID {
		return cron.ErrNotYours
	}
	_, err = s.db.Exec(`UPDATE cron_watches SET message = ? WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`, message, cronHome(ns), name)
	return err
}

func (s *CronBackend) GetWatch(_ shape.Added, ns uint, name string) (*cron.Watch, error) {
	w, err := cronScanWatch(s.db.QueryRow(`SELECT `+cronWatchCols+` FROM cron_watches WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`, cronHome(ns), name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, cron.ErrNotFound
	}
	return w, err
}

func (s *CronBackend) listWatches(where string, args ...any) ([]cron.Watch, error) {
	rows, err := s.db.Query(`SELECT `+cronWatchCols+` FROM cron_watches WHERE deleted_at IS NULL`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cron.Watch
	for rows.Next() {
		w, err := cronScanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *CronBackend) ListWatches(shape.Added) ([]cron.Watch, error) {
	return s.listWatches(` ORDER BY name, id`)
}

func (s *CronBackend) DeleteWatch(_ shape.Added, ns uint, name string) error {
	_, err := s.db.Exec(`UPDATE cron_watches SET deleted_at = ? WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`,
		cronEnc(s.now()), cronHome(ns), name)
	return err
}

func (s *CronBackend) MoveWatch(_ shape.Added, fromNS uint, fromName string, toNS uint, toName string) error {
	return s.inTx(func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRow(`SELECT id FROM cron_watches WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`, cronHome(fromNS), fromName).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return cron.ErrNotFound
		}
		if err != nil {
			return err
		}
		var held int
		if err := tx.QueryRow(`SELECT count(*) FROM cron_watches WHERE deleted_at IS NULL AND `+cronInHome+` AND name = ?`, cronHome(toNS), toName).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return cron.ErrExists
		}
		// A deleted watch that held the name is gone for good now: the row is the
		// new home's to take.
		if _, err := tx.Exec(`DELETE FROM cron_watches WHERE deleted_at IS NOT NULL AND `+cronInHome+` AND name = ?`, cronHome(toNS), toName); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE cron_watches SET namespace_id = ?, name = ?, updated_at = ? WHERE id = ?`, cronHome(toNS), toName, cronEnc(s.now()), id)
		return err
	})
}

func (s *CronBackend) CountEnabled(shape.Added) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM cron_watches WHERE deleted_at IS NULL AND enabled = 1`).Scan(&n)
	return n, err
}

func (s *CronBackend) ListEnabled(context.Context) ([]cron.Watch, error) {
	return s.listWatches(` AND enabled = 1 ORDER BY id`)
}

// cronChanged reports ErrNotFound when an update by id touched no row.
func cronChanged(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return cron.ErrNotFound
	}
	return nil
}

func (s *CronBackend) RecordRun(_ context.Context, id uint, at time.Time, status string, resetStrikes bool) error {
	q := `UPDATE cron_watches SET last_run_at = ?, last_status = ?`
	if resetStrikes {
		q += `, consecutive_strikes = 0`
	}
	return cronChanged(s.db.Exec(q+` WHERE id = ?`, cronEnc(at), status, id))
}

func (s *CronBackend) SetLoweredPlan(_ context.Context, id uint, plan []byte) error {
	return cronChanged(s.db.Exec(`UPDATE cron_watches SET lowered_plan = ? WHERE id = ?`, cronPlanArg(plan), id))
}

func (s *CronBackend) RecordStrike(_ context.Context, id uint, limit int) (bool, error) {
	var enabled int
	err := s.db.QueryRow(`UPDATE cron_watches
		SET consecutive_strikes = consecutive_strikes + 1,
		    enabled = enabled AND consecutive_strikes + 1 < ?
		WHERE id = ? RETURNING enabled`, limit, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, cron.ErrNotFound
	}
	return enabled == 0, err
}

func (s *CronBackend) Disable(_ context.Context, id uint) (bool, error) {
	res, err := s.db.Exec(`UPDATE cron_watches SET enabled = 0 WHERE id = ? AND enabled = 1`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// --- results ---

func (s *CronBackend) SaveResults(rs []cron.Result) error {
	if len(rs) == 0 {
		return nil
	}
	return s.inTx(func(tx *sql.Tx) error {
		for _, r := range rs {
			if _, err := tx.Exec(`INSERT INTO cron_results (cron_watch_id, ran_at, stdout, stderr, exit_code, duration_ms, value, tag, text_value)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				r.WatchID, cronEnc(r.RanAt), r.Stdout, r.Stderr, r.ExitCode, r.DurationMs, cronNullFloat(r.Value), r.Tag, cronNullStr(r.TextValue)); err != nil {
				return err
			}
		}
		return nil
	})
}

const cronResultCols = `id, cron_watch_id, ran_at, stdout, stderr, exit_code, duration_ms, value, tag, text_value`

func cronScanResults(rows *sql.Rows) ([]cron.Result, error) {
	defer rows.Close()
	var out []cron.Result
	for rows.Next() {
		var (
			r     cron.Result
			ran   string
			value sql.NullFloat64
			text  sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.WatchID, &ran, &r.Stdout, &r.Stderr, &r.ExitCode, &r.DurationMs, &value, &r.Tag, &text); err != nil {
			return nil, err
		}
		var err error
		if r.RanAt, err = cronDec(ran); err != nil {
			return nil, err
		}
		if value.Valid {
			r.Value = &value.Float64
		}
		if text.Valid {
			r.TextValue = &text.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *CronBackend) ListResults(watchID uint, limit int, tag string) ([]cron.Result, error) {
	q := `SELECT ` + cronResultCols + ` FROM cron_results WHERE cron_watch_id = ?`
	args := []any{watchID}
	if tag != "" {
		q += ` AND tag = ?`
		args = append(args, tag)
	}
	rows, err := s.db.Query(q+` ORDER BY ran_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return cronScanResults(rows)
}

func (s *CronBackend) LatestRun(watchID uint) ([]cron.Result, error) {
	rows, err := s.db.Query(`SELECT `+cronResultCols+` FROM cron_results
		WHERE cron_watch_id = ?1 AND (value IS NOT NULL OR text_value IS NOT NULL)
		  AND ran_at = (SELECT max(ran_at) FROM cron_results WHERE cron_watch_id = ?1 AND (value IS NOT NULL OR text_value IS NOT NULL))
		ORDER BY value DESC NULLS LAST, tag ASC`, watchID)
	if err != nil {
		return nil, err
	}
	return cronScanResults(rows)
}

func (s *CronBackend) CountResults(watchID uint, since time.Time, tag string, nonEmpty bool) (int64, error) {
	q := `SELECT count(*) FROM cron_results WHERE cron_watch_id = ? AND ran_at >= ?`
	args := []any{watchID, cronEnc(since)}
	if tag != "" {
		q += ` AND tag = ?`
		args = append(args, tag)
	}
	if nonEmpty {
		q += ` AND stdout <> ''`
	}
	var n int64
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func (s *CronBackend) NumericEnds(watchID uint, since time.Time) (cron.Ends, error) {
	var e cron.Ends
	const from = ` FROM cron_results WHERE cron_watch_id = ?1 AND ran_at >= ?2 AND value IS NOT NULL`
	err := s.db.QueryRow(`SELECT count(*),
		COALESCE((SELECT value`+from+` ORDER BY ran_at ASC, id ASC LIMIT 1), 0),
		COALESCE((SELECT value`+from+` ORDER BY ran_at DESC, id DESC LIMIT 1), 0)`+from, watchID, cronEnc(since)).
		Scan(&e.N, &e.First, &e.Last)
	return e, err
}

func (s *CronBackend) NumericPoints(watchID uint, since time.Time) ([]cron.Point, error) {
	rows, err := s.db.Query(`SELECT ran_at, value FROM cron_results
		WHERE cron_watch_id = ? AND ran_at >= ? AND value IS NOT NULL ORDER BY ran_at, id`, watchID, cronEnc(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cron.Point
	for rows.Next() {
		var (
			ran string
			p   cron.Point
		)
		if err := rows.Scan(&ran, &p.Value); err != nil {
			return nil, err
		}
		if p.At, err = cronDec(ran); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *CronBackend) WindowStats(watchID uint, since time.Time, tag string) (cron.WindowStats, error) {
	q := `SELECT count(*), COALESCE(SUM(value), 0), COALESCE(SUM(value * value), 0), COALESCE(MIN(value), 0), COALESCE(MAX(value), 0)
		FROM cron_results WHERE cron_watch_id = ? AND ran_at >= ? AND value IS NOT NULL`
	args := []any{watchID, cronEnc(since)}
	if tag != "" {
		q += ` AND tag = ?`
		args = append(args, tag)
	}
	var st cron.WindowStats
	err := s.db.QueryRow(q, args...).Scan(&st.Count, &st.Sum, &st.SumSq, &st.Min, &st.Max)
	return st, err
}

func (s *CronBackend) Series(watchID uint, start, end time.Time) (cron.Series, error) {
	out := cron.Series{Granularity: "raw", Start: start, End: end}
	if !start.Before(end) {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT ran_at, tag, value FROM cron_results
		WHERE cron_watch_id = ? AND ran_at >= ? AND ran_at < ? AND value IS NOT NULL ORDER BY ran_at, tag, id`,
		watchID, cronEnc(start), cronEnc(end))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ran string
			b   cron.Bucket
			v   float64
		)
		if err := rows.Scan(&ran, &b.Tag, &v); err != nil {
			return out, err
		}
		if b.Start, err = cronDec(ran); err != nil {
			return out, err
		}
		b.Count, b.Sum, b.SumSq, b.Min, b.Max = 1, v, v*v, v, v
		out.Buckets = append(out.Buckets, b)
	}
	return out, rows.Err()
}

// --- alert incidents ---

func (s *CronBackend) OpenOrTouchIncident(watchID uint, tag string) (*cron.Incident, error) {
	now := s.now().UTC()
	var inc *cron.Incident
	err := s.inTx(func(tx *sql.Tx) error {
		const open = `SELECT id, opened_at FROM alert_incidents WHERE cron_watch_id = ? AND tag = ? AND closed_at IS NULL`
		var id, opened string
		err := tx.QueryRow(open, watchID, tag).Scan(&id, &opened)
		if err == nil {
			if _, err := tx.Exec(`UPDATE alert_incidents SET last_seen_at = ? WHERE id = ?`, cronEnc(now), id); err != nil {
				return err
			}
			o, err := cronDec(opened)
			inc = &cron.Incident{ID: id, WatchID: watchID, Tag: tag, OpenedAt: o, LastSeenAt: now}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id = uuid.New().String()
		if _, err := tx.Exec(`INSERT INTO alert_incidents (id, cron_watch_id, tag, opened_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
			id, watchID, tag, cronEnc(now), cronEnc(now)); err != nil {
			return err
		}
		inc = &cron.Incident{ID: id, WatchID: watchID, Tag: tag, OpenedAt: now, LastSeenAt: now}
		return nil
	})
	return inc, err
}

func (s *CronBackend) CloseIncident(watchID uint, tag string) error {
	_, err := s.db.Exec(`UPDATE alert_incidents SET closed_at = ? WHERE cron_watch_id = ? AND tag = ? AND closed_at IS NULL`,
		cronEnc(s.now()), watchID, tag)
	return err
}

func (s *CronBackend) IncidentDeliveries(incidentID string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT target FROM alert_incident_deliveries WHERE incident_id = ?`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := map[string]bool{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		done[t] = true
	}
	return done, rows.Err()
}

func (s *CronBackend) RecordDeliveries(incidentID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	now := cronEnc(s.now())
	return s.inTx(func(tx *sql.Tx) error {
		for _, t := range targets {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO alert_incident_deliveries (incident_id, target, delivered_at) VALUES (?, ?, ?)`,
				incidentID, t, now); err != nil {
				return err
			}
		}
		return nil
	})
}
