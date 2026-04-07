package configstore

import (
	"crypto/rand"
	"fmt"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Customer struct {
	gorm.Model
	Name        string       `gorm:"uniqueIndex;not null"`
	Accounts    []Account    `gorm:"foreignKey:CustomerID"`
	AgentTokens []AgentToken `gorm:"foreignKey:CustomerID"`
}

// AgentToken authenticates a weftd agent and maps it to a customer.
// Token format: wft_<random> (e.g., wft_abc123def456)
type AgentToken struct {
	gorm.Model
	CustomerID uint     `gorm:"not null"`
	Customer   Customer `gorm:"foreignKey:CustomerID"`
	Token      string   `gorm:"uniqueIndex;not null"`
	Label      string   // human-readable label (e.g., "vimgym-prod")
	App        string   // app name this token authorizes (e.g., "vimgym")
	Revoked    bool     `gorm:"not null;default:false"`
}

type Account struct {
	gorm.Model
	CustomerID uint      `gorm:"not null"`
	Customer   Customer  `gorm:"foreignKey:CustomerID"`
	AWSAccount string    `gorm:"not null"`
	RoleARN    string    `gorm:"not null"`
	ExternalID string
	LogGroups  []LogGroup `gorm:"foreignKey:AccountID"`
}

type LogGroup struct {
	gorm.Model
	AccountID    uint    `gorm:"not null;uniqueIndex:idx_account_app"`
	Account      Account `gorm:"foreignKey:AccountID"`
	App          string  `gorm:"not null;uniqueIndex:idx_account_app"`
	LogGroupName string  `gorm:"not null"`
	Region       string  `gorm:"not null;default:us-east-2"`
	SchemaYAML   string  `gorm:"not null"`
	PollInterval string  `gorm:"not null;default:30s"`
	Enabled      bool    `gorm:"not null;default:true"`
	LastPollAt   string
	LastToken    string
}

// ShardRecord persists shard registry entries across coordinator restarts.
type ShardRecord struct {
	ID           string `gorm:"primaryKey"`
	Apps         string `gorm:"not null"` // JSON array: ["vimgym","callbox"]
	WindowStart  string `gorm:"not null"` // RFC3339
	WindowEnd    string `gorm:"not null"` // RFC3339
	State        string `gorm:"not null"` // INGESTING/READ_ONLY/ARCHIVED
	Addr         string                   // gRPC address for live shards
	InstanceID   string                   // EC2 instance ID
	ParquetPath  string                   // Parquet file path for archived shards
	Tier         string                   // instance tier (small/medium/large)
	RowCount     int64  `gorm:"default:0"`
	CreatedAt    string `gorm:"not null"` // RFC3339
}

type Store struct {
	db *gorm.DB
}

func Open(path string) (*Store, error) {
	db, err := gorm.Open(sqlite.Open(path+"?_journal_mode=WAL"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&Customer{}, &Account{}, &LogGroup{}, &AgentToken{}, &ShardRecord{}); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

// FindOrCreateCustomer returns the customer, creating if needed.
func (s *Store) FindOrCreateCustomer(name string) (*Customer, error) {
	var c Customer
	result := s.db.Where("name = ?", name).FirstOrCreate(&c, Customer{Name: name})
	return &c, result.Error
}

// FindOrCreateAccount returns the account, creating or updating if needed.
func (s *Store) FindOrCreateAccount(customerID uint, awsAccount, roleARN, externalID string) (*Account, error) {
	var a Account
	result := s.db.Where("customer_id = ? AND aws_account = ?", customerID, awsAccount).First(&a)
	if result.Error == gorm.ErrRecordNotFound {
		a = Account{
			CustomerID: customerID,
			AWSAccount: awsAccount,
			RoleARN:    roleARN,
			ExternalID: externalID,
		}
		result = s.db.Create(&a)
	} else if result.Error == nil {
		result = s.db.Model(&a).Updates(Account{RoleARN: roleARN, ExternalID: externalID})
	}
	return &a, result.Error
}

// AddLogGroup creates or updates a log group source.
func (s *Store) AddLogGroup(accountID uint, app, logGroupName, region, schemaYAML, pollInterval string) error {
	var lg LogGroup
	result := s.db.Where("account_id = ? AND app = ?", accountID, app).First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		lg = LogGroup{
			AccountID:    accountID,
			App:          app,
			LogGroupName: logGroupName,
			Region:       region,
			SchemaYAML:   schemaYAML,
			PollInterval: pollInterval,
			Enabled:      true,
		}
		return s.db.Create(&lg).Error
	}
	return s.db.Model(&lg).Updates(LogGroup{
		LogGroupName: logGroupName,
		Region:       region,
		SchemaYAML:   schemaYAML,
		PollInterval: pollInterval,
	}).Error
}

// RemoveLogGroup deletes a log group by app name.
func (s *Store) RemoveLogGroup(app string) error {
	return s.db.Where("app = ?", app).Delete(&LogGroup{}).Error
}

// LogGroupWithAccount is a joined result for the poller.
type LogGroupWithAccount struct {
	LogGroup
	CustomerName string
	RoleARN      string
	ExternalID   string
	AWSAccount   string
}

// EnabledLogGroups returns all enabled log groups with customer/account info.
func (s *Store) EnabledLogGroups() ([]LogGroupWithAccount, error) {
	var results []LogGroupWithAccount
	err := s.db.
		Table("log_groups").
		Select("log_groups.*, customers.name as customer_name, accounts.role_arn, accounts.external_id, accounts.aws_account").
		Joins("JOIN accounts ON log_groups.account_id = accounts.id").
		Joins("JOIN customers ON accounts.customer_id = customers.id").
		Where("log_groups.enabled = ?", true).
		Find(&results).Error
	return results, err
}

// AllLogGroups returns all log groups with customer/account info.
func (s *Store) AllLogGroups() ([]LogGroupWithAccount, error) {
	var results []LogGroupWithAccount
	err := s.db.
		Table("log_groups").
		Select("log_groups.*, customers.name as customer_name, accounts.role_arn, accounts.external_id, accounts.aws_account").
		Joins("JOIN accounts ON log_groups.account_id = accounts.id").
		Joins("JOIN customers ON accounts.customer_id = customers.id").
		Order("customers.name, log_groups.app").
		Find(&results).Error
	return results, err
}

// UpdatePollState saves the last poll timestamp and pagination token.
func (s *Store) UpdatePollState(app, lastPollAt, lastToken string) error {
	return s.db.Model(&LogGroup{}).Where("app = ?", app).
		Updates(map[string]any{"last_poll_at": lastPollAt, "last_token": lastToken}).Error
}

// AppNames returns distinct app names from enabled log groups.
func (s *Store) AppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&LogGroup{}).Where("enabled = ?", true).Distinct().Pluck("app", &names).Error
	return names, err
}

// --- Agent Tokens ---

// CreateAgentToken generates a new token for a customer and app.
func (s *Store) CreateAgentToken(customerID uint, label, app string) (*AgentToken, error) {
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	t := AgentToken{
		CustomerID: customerID,
		Token:      token,
		Label:      label,
		App:        app,
	}
	if err := s.db.Create(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// AgentAppNames returns distinct app names from active (non-revoked) agent tokens.
func (s *Store) AgentAppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&AgentToken{}).
		Where("revoked = ? AND app != ''", false).
		Distinct().Pluck("app", &names).Error
	return names, err
}

// ValidateAgentToken checks a token and returns the associated customer name.
// Returns nil if the token is invalid or revoked.
func (s *Store) ValidateAgentToken(token string) (*Customer, error) {
	var t AgentToken
	result := s.db.Preload("Customer").Where("token = ? AND revoked = ?", token, false).First(&t)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &t.Customer, nil
}

// RevokeAgentToken disables a token.
func (s *Store) RevokeAgentToken(token string) error {
	return s.db.Model(&AgentToken{}).Where("token = ?", token).Update("revoked", true).Error
}

// ListAgentTokens returns all tokens for a customer.
func (s *Store) ListAgentTokens(customerID uint) ([]AgentToken, error) {
	var tokens []AgentToken
	err := s.db.Where("customer_id = ?", customerID).Find(&tokens).Error
	return tokens, err
}

// --- Shard Registry ---

// SaveShard upserts a shard record.
func (s *Store) SaveShard(rec *ShardRecord) error {
	return s.db.Save(rec).Error
}

// DeleteShard removes a shard record.
func (s *Store) DeleteShard(id string) error {
	return s.db.Delete(&ShardRecord{}, "id = ?", id).Error
}

// ListShards returns all shard records.
func (s *Store) ListShards() ([]ShardRecord, error) {
	var records []ShardRecord
	err := s.db.Find(&records).Error
	return records, err
}

// ListShardsByState returns shard records with the given state.
func (s *Store) ListShardsByState(state string) ([]ShardRecord, error) {
	var records []ShardRecord
	err := s.db.Where("state = ?", state).Find(&records).Error
	return records, err
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("wft_%x", b), nil
}
