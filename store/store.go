package store

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store manages all weft PostgreSQL state in a single unified package.
type Store struct {
	db *gorm.DB
}

// Open connects to the database and runs migrations for all models.
// Usage tables (usage_hourly, usage_daily) are managed via SQL migrations, not AutoMigrate.
func Open(dsn string) (*Store, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&Customer{}, &Namespace{}, &Account{}, &LogGroup{}, &AgentToken{},
		&ShardRecord{}, &Subscription{},
		&ShellFunction{}, &CronWatch{}, &CronResult{}, &NotifyConfig{},
		&User{}, &MagicLink{}, &SavedQuery{}, &TeamMember{},
		&AlertIncident{}, &DeviceCode{}, &NamespacePermission{},
		&PendingSource{},
	); err != nil {
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
	s.db.Exec("TRUNCATE TABLE pending_sources, cron_results, cron_watches, shell_functions, agent_tokens, notify_configs, namespace_permissions, namespaces, alert_incidents, device_codes, magic_links, saved_queries, team_members, shard_records, subscriptions, users, customers CASCADE")
}

// Close releases the database connection.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}
