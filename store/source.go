package store

import "gorm.io/gorm"

type Account struct {
	gorm.Model
	CustomerID uint      `gorm:"not null"`
	Customer   Customer  `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	AWSAccount string    `gorm:"not null"`
	RoleARN    string    `gorm:"not null"`
	ExternalID string
	LogGroups  []LogGroup `gorm:"foreignKey:AccountID;constraint:OnDelete:CASCADE"`
}

type LogGroup struct {
	gorm.Model
	AccountID    uint    `gorm:"not null;uniqueIndex:idx_account_app"`
	Account      Account `gorm:"foreignKey:AccountID;constraint:OnDelete:CASCADE"`
	App          string  `gorm:"not null;uniqueIndex:idx_account_app"`
	LogGroupName string  `gorm:"not null"`
	Region       string  `gorm:"not null;default:us-east-2"`
	SchemaYAML   string  `gorm:"not null"`
	PollInterval string  `gorm:"not null;default:30s"`
	Enabled      bool    `gorm:"not null;default:true"`
	LastPollAt   string
	LastToken    string
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
