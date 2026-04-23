package store

import (
	"fmt"
	"log"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store manages all weft PostgreSQL state in a single unified package.
type Store struct {
	db *gorm.DB

	// Test-only: raw magic-link tokens indexed by email. Populated by
	// CreateMagicLink; read by GetLastMagicLinkToken. The tokens
	// themselves are hashed before being persisted, so this map is the
	// only way an in-process test can retrieve the raw value after
	// sending it through the handler. In production nothing reads from
	// this map, and tokens are cleared every 15 minutes by normal
	// consumption / expiry.
	rawMagicLinkMu     sync.Mutex
	rawMagicLinkTokens map[string]string
}

// Open connects to the database and ensures all goose migrations have been
// applied. Schema is authoritative from the .sql files under store/migrations;
// we no longer use GORM AutoMigrate.
func Open(dsn string) (*Store, error) {
	if err := Migrate(dsn); err != nil {
		return nil, err
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.backfillAlertThresholds(); err != nil {
		return nil, fmt.Errorf("backfill alert thresholds: %w", err)
	}
	return s, nil
}

// backfillAlertThresholds populates (alert_condition, alert_threshold)
// for watches that predate migration 027. The columns were added
// nullable so the migration itself is instant; this second pass walks
// rows with NULL values, re-compiles the pipeline, and writes the
// projection. Idempotent: once every eligible row has both columns
// set, the initial SELECT returns zero and the function is a no-op.
// Unparseable pipelines are skipped so a bad row can't block startup.
func (s *Store) backfillAlertThresholds() error {
	type row struct {
		ID       uint
		Pipeline string
	}
	var rows []row
	err := s.db.Raw(`
		SELECT id, pipeline
		FROM cron_watches
		WHERE deleted_at IS NULL
		  AND alert_condition IS NULL
		  AND alert_threshold IS NULL
	`).Scan(&rows).Error
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	backfilled := 0
	for _, r := range rows {
		compiled, err := CompileWatchPipeline(r.Pipeline)
		if err != nil {
			// A pipeline that no longer parses (or has multiple
			// alerts, etc.) is a pre-existing bad row — don't let
			// it block server startup. Leave the columns NULL; the
			// dashboard falls back to no threshold line.
			continue
		}
		if compiled.AlertCondition == nil || compiled.AlertThreshold == nil {
			// Watch has no numeric-cutoff alert. Nothing to
			// project; leave NULL.
			continue
		}
		if err := s.db.Exec(`
			UPDATE cron_watches
			SET alert_condition = ?, alert_threshold = ?
			WHERE id = ?`,
			*compiled.AlertCondition, *compiled.AlertThreshold, r.ID,
		).Error; err != nil {
			return err
		}
		backfilled++
	}
	if backfilled > 0 {
		log.Printf("store: backfilled alert_condition/alert_threshold on %d cron_watches", backfilled)
	}
	return nil
}

// BeginTest starts a transaction and returns a Store that uses it.
// Call the returned rollback function in t.Cleanup to undo all changes.
func (s *Store) BeginTest() (*Store, func()) {
	tx := s.db.Begin()
	return &Store{db: tx}, func() { tx.Rollback() }
}

// TruncateTestTables removes all data from GORM-managed tables. For tests only.
func (s *Store) TruncateTestTables() {
	s.db.Exec("TRUNCATE TABLE cron_results, cron_watches, shell_functions, agent_tokens, notify_configs, namespace_permissions, namespaces, alert_incidents, device_codes, magic_links, saved_queries, team_members, shard_records, subscriptions, users, customers CASCADE")
}

// Close releases the database connection.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}
