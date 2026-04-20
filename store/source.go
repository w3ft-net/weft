package store

import (
	"strings"

	"gorm.io/gorm"
)

// AWSAccount is a cross-account AWS registration a customer uses to let the
// coordinator pull CloudWatch logs. Exclusively CloudWatch-shaped: AccountID
// is the 12-digit AWS account number, RoleARN is the role the coordinator
// assumes, ExternalID is the STS external-id secret. Weftd sources don't
// have one (their log_groups.aws_account_id is NULL).
type AWSAccount struct {
	gorm.Model
	CustomerID uint       `gorm:"not null"`
	Customer   Customer   `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	AccountID  string     `gorm:"not null"` // 12-digit AWS account number
	RoleARN    string     `gorm:"not null"`
	ExternalID string
	LogGroups  []LogGroup `gorm:"foreignKey:AWSAccountID;constraint:OnDelete:CASCADE"`
}

// LogGroupSourceType identifies where a log_groups row's rows come from.
// cloudwatch: coordinator polls CloudWatch FilterLogEvents on a schedule.
// weftd:      weftd agents push rows via gRPC IngestStream; no polling.
type LogGroupSourceType string

const (
	SourceTypeCloudWatch LogGroupSourceType = "cloudwatch"
	SourceTypeWeftd      LogGroupSourceType = "weftd"
)

// LogGroup represents a configured log source. Ownership is pinned through
// NamespaceID (required); AWSAccountID is only populated for cloudwatch
// sources since weftd sources don't have an AWS account.
type LogGroup struct {
	gorm.Model
	NamespaceID  uint               `gorm:"not null;uniqueIndex:idx_ns_app"`
	Namespace    Namespace          `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	AWSAccountID *uint              // nullable; required for cloudwatch, nil for weftd
	AWSAccount   *AWSAccount        `gorm:"foreignKey:AWSAccountID"`
	App          string             `gorm:"not null;uniqueIndex:idx_ns_app"`
	SourceType   LogGroupSourceType `gorm:"type:log_group_source_type;not null"`
	LogGroupName string             `gorm:"not null"`
	Region       string             `gorm:"not null;default:us-east-2"`
	SchemaYAML   string             `gorm:"not null"`
	PollInterval string             `gorm:"not null;default:30s"`
	Enabled      bool               `gorm:"not null;default:true"`
	LastPollAt   string
	LastToken    string
}

// FindOrCreateAWSAccount returns the AWS-account registration, creating or
// updating credentials if needed. accountID is the 12-digit AWS account
// number; roleARN is the cross-account role the coordinator will assume.
func (s *Store) FindOrCreateAWSAccount(customerID uint, accountID, roleARN, externalID string) (*AWSAccount, error) {
	var a AWSAccount
	result := s.db.Where("customer_id = ? AND account_id = ?", customerID, accountID).First(&a)
	if result.Error == gorm.ErrRecordNotFound {
		a = AWSAccount{
			CustomerID: customerID,
			AccountID:  accountID,
			RoleARN:    roleARN,
			ExternalID: externalID,
		}
		result = s.db.Create(&a)
	} else if result.Error == nil {
		result = s.db.Model(&a).Updates(AWSAccount{RoleARN: roleARN, ExternalID: externalID})
	}
	return &a, result.Error
}

// AddLogGroup creates or updates a log group source. Ownership is keyed on
// (namespace_id, app). awsAccountID must be non-nil for cloudwatch sources
// and nil for weftd sources — the caller is trusted to match sourceType to
// account nullability.
func (s *Store) AddLogGroup(namespaceID uint, awsAccountID *uint, sourceType LogGroupSourceType, app, logGroupName, region, schemaYAML, pollInterval string) error {
	var lg LogGroup
	result := s.db.Where("namespace_id = ? AND app = ?", namespaceID, app).First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		lg = LogGroup{
			NamespaceID:  namespaceID,
			AWSAccountID: awsAccountID,
			SourceType:   sourceType,
			App:          app,
			LogGroupName: logGroupName,
			Region:       region,
			SchemaYAML:   schemaYAML,
			PollInterval: pollInterval,
			Enabled:      true,
		}
		return s.db.Create(&lg).Error
	}
	return s.db.Model(&lg).Updates(map[string]any{
		"aws_account_id": awsAccountID,
		"source_type":    sourceType,
		"log_group_name": logGroupName,
		"region":         region,
		"schema_yaml":    schemaYAML,
		"poll_interval":  pollInterval,
	}).Error
}

// CustomerHasSource reports whether the customer owns the namespace
// referenced by the leading '/'-segment of app. An empty app means
// "all sources for this customer" (cat warp.log) and is always allowed —
// the customer WHERE clause already gates the result set.
func (s *Store) CustomerHasSource(customerID uint, app string) (bool, error) {
	if app == "" {
		return true, nil
	}
	ns := app
	if i := strings.IndexByte(app, '/'); i >= 0 {
		ns = app[:i]
	}
	var count int64
	err := s.db.Model(&Namespace{}).
		Where("customer_id = ? AND name = ?", customerID, ns).
		Count(&count).Error
	return count > 0, err
}

// RemoveLogGroup deletes a log group by (namespace_id, app). Scoped to a
// namespace so one customer can't delete another customer's source by
// guessing an app name.
//
// Uses Unscoped() to hard-delete rather than GORM's default soft-delete.
// Without it, the row sits in the table with deleted_at != NULL but
// still participates in the plain unique index idx_ns_app on
// (namespace_id, app), so a later re-add of the same source would
// trip a 23505 unique-constraint violation. "Remove" means gone.
func (s *Store) RemoveLogGroup(namespaceID uint, app string) error {
	return s.db.Unscoped().Where("namespace_id = ? AND app = ?", namespaceID, app).Delete(&LogGroup{}).Error
}

// LogGroupWithAWSAccount is a joined result for the poller and list paths.
// AWS-account-scoped fields (RoleARN, ExternalID, AccountID) are only
// populated for cloudwatch sources; weftd sources leave them empty since
// they don't have an AWS account.
type LogGroupWithAWSAccount struct {
	LogGroup
	CustomerName  string
	NamespaceName string
	RoleARN       string
	ExternalID    string
	AccountID     string // 12-digit AWS account number
}

// EnabledLogGroups returns all enabled cloudwatch log groups with
// customer/account info. Weftd sources are excluded because the poller
// doesn't drive them.
func (s *Store) EnabledLogGroups() ([]LogGroupWithAWSAccount, error) {
	var results []LogGroupWithAWSAccount
	err := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        customers.name as customer_name,
		        namespaces.name as namespace_name,
		        aws_accounts.role_arn,
		        aws_accounts.external_id,
		        aws_accounts.account_id`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN customers ON namespaces.customer_id = customers.id").
		Joins("JOIN aws_accounts ON log_groups.aws_account_id = aws_accounts.id").
		Where("log_groups.enabled = ? AND log_groups.source_type = ?", true, SourceTypeCloudWatch).
		Find(&results).Error
	return results, err
}

// AllLogGroups returns all log groups with customer/namespace info. AWS
// account fields are populated for cloudwatch rows and left empty for
// weftd rows via the LEFT JOIN.
func (s *Store) AllLogGroups() ([]LogGroupWithAWSAccount, error) {
	var results []LogGroupWithAWSAccount
	err := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        customers.name as customer_name,
		        namespaces.name as namespace_name,
		        aws_accounts.role_arn,
		        aws_accounts.external_id,
		        aws_accounts.account_id`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN customers ON namespaces.customer_id = customers.id").
		Joins("LEFT JOIN aws_accounts ON log_groups.aws_account_id = aws_accounts.id").
		Order("customers.name, namespaces.name, log_groups.app").
		Find(&results).Error
	return results, err
}

// UpdatePollState saves the last poll timestamp and pagination token for
// the given log_groups row id. Keyed by id rather than app since app is
// only unique within a namespace.
func (s *Store) UpdatePollState(id uint, lastPollAt, lastToken string) error {
	return s.db.Model(&LogGroup{}).Where("id = ?", id).
		Updates(map[string]any{"last_poll_at": lastPollAt, "last_token": lastToken}).Error
}

// AppNames returns distinct app names from enabled log groups.
func (s *Store) AppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&LogGroup{}).Where("enabled = ?", true).Distinct().Pluck("app", &names).Error
	return names, err
}
