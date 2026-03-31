package configstore

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Store is a SQLite-backed configuration store for the coordinator.
// It holds log source definitions (app → CloudWatch log group + schema)
// and customer configuration.
type Store struct {
	db *sql.DB
}

// Open opens or creates a SQLite database at the given path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open config db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS sources (
			app           TEXT PRIMARY KEY,
			customer      TEXT NOT NULL,
			log_group     TEXT NOT NULL,
			region        TEXT NOT NULL DEFAULT 'us-east-2',
			schema_yaml   TEXT NOT NULL,
			poll_interval TEXT NOT NULL DEFAULT '30s',
			enabled       BOOLEAN NOT NULL DEFAULT 1,
			last_poll_at  TEXT,
			last_token    TEXT
		);
	`)
	return err
}

// Source represents a log source to poll from CloudWatch.
type Source struct {
	App          string
	Customer     string
	LogGroup     string
	Region       string
	SchemaYAML   string
	PollInterval string
	Enabled      bool
	LastPollAt   string // RFC3339 timestamp of last successful poll
	LastToken    string // CloudWatch nextForwardToken for pagination
}

// AddSource adds or updates a log source.
func (s *Store) AddSource(src Source) error {
	_, err := s.db.Exec(`
		INSERT INTO sources (app, customer, log_group, region, schema_yaml, poll_interval, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app) DO UPDATE SET
			customer = excluded.customer,
			log_group = excluded.log_group,
			region = excluded.region,
			schema_yaml = excluded.schema_yaml,
			poll_interval = excluded.poll_interval,
			enabled = excluded.enabled
	`, src.App, src.Customer, src.LogGroup, src.Region, src.SchemaYAML, src.PollInterval, src.Enabled)
	return err
}

// RemoveSource deletes a log source.
func (s *Store) RemoveSource(app string) error {
	_, err := s.db.Exec("DELETE FROM sources WHERE app = ?", app)
	return err
}

// ListSources returns all configured sources.
func (s *Store) ListSources() ([]Source, error) {
	rows, err := s.db.Query("SELECT app, customer, log_group, region, schema_yaml, poll_interval, enabled, COALESCE(last_poll_at, ''), COALESCE(last_token, '') FROM sources")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.App, &src.Customer, &src.LogGroup, &src.Region, &src.SchemaYAML, &src.PollInterval, &src.Enabled, &src.LastPollAt, &src.LastToken); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSource returns a single source by app name.
func (s *Store) GetSource(app string) (*Source, error) {
	var src Source
	err := s.db.QueryRow("SELECT app, customer, log_group, region, schema_yaml, poll_interval, enabled, COALESCE(last_poll_at, ''), COALESCE(last_token, '') FROM sources WHERE app = ?", app).
		Scan(&src.App, &src.Customer, &src.LogGroup, &src.Region, &src.SchemaYAML, &src.PollInterval, &src.Enabled, &src.LastPollAt, &src.LastToken)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &src, nil
}

// EnabledSources returns only enabled sources.
func (s *Store) EnabledSources() ([]Source, error) {
	rows, err := s.db.Query("SELECT app, customer, log_group, region, schema_yaml, poll_interval, enabled, COALESCE(last_poll_at, ''), COALESCE(last_token, '') FROM sources WHERE enabled = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.App, &src.Customer, &src.LogGroup, &src.Region, &src.SchemaYAML, &src.PollInterval, &src.Enabled, &src.LastPollAt, &src.LastToken); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// UpdatePollState saves the last poll timestamp and pagination token.
func (s *Store) UpdatePollState(app, lastPollAt, lastToken string) error {
	_, err := s.db.Exec("UPDATE sources SET last_poll_at = ?, last_token = ? WHERE app = ?", lastPollAt, lastToken, app)
	return err
}
