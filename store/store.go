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
		&Customer{}, &App{}, &Account{}, &LogGroup{}, &AgentToken{},
		&ShardRecord{}, &Subscription{},
		&ShellFunction{}, &CronWatch{}, &CronResult{}, &NotifyConfig{},
		&User{}, &MagicLink{}, &SavedQuery{}, &TeamMember{},
		&AlertIncident{},
	); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}
