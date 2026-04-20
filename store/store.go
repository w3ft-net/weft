package store

import (
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
	return &Store{db: db}, nil
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
