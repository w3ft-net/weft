package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// AWSAccount is a cross-account AWS registration a customer uses to let the
// coordinator pull CloudWatch logs. Exclusively CloudWatch-shaped: AccountID
// is the 12-digit AWS account number, RoleARN is the role the coordinator
// assumes, ExternalID is the STS external-id secret. Weftd sources don't
// have one (their log_groups.aws_account_id is NULL).
type AWSAccount struct {
	gorm.Model
	CustomerID uint     `gorm:"not null"`
	Customer   Customer `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	AccountID  string   `gorm:"not null"` // 12-digit AWS account number
	RoleARN    string   `gorm:"not null"`
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

// LogGroupStatus tracks whether a human has approved the schema.
// pending:    weftd has sent a sample but nobody's configured the schema yet
//
//	(Sample/Detection hold the auto-detected candidates). The
//	coordinator ignores pending rows — no polling, no ingest.
//
// configured: schema_yaml is valid and the source is live.
// status is orthogonal to source_type: a cloudwatch row today always
// starts configured (the add-source UI commits schema in one shot), but
// the schema supports a pending cloudwatch row for a future auto-discovery
// UX.
type LogGroupStatus string

// SampleTarget is the line count a full sample carries. The agent owns
// the real accumulation; this is the canonical value the coordinator and
// UIs use to report progress ("N/SampleTarget").
const SampleTarget = 50

const (
	// StatusSampling: the agent has announced the source but is still
	// accumulating toward a full sample. Not yet configurable.
	StatusSampling   LogGroupStatus = "sampling"
	StatusPending    LogGroupStatus = "pending"
	StatusConfigured LogGroupStatus = "configured"
)

// LogGroup represents a log source — pending or configured. Ownership is
// pinned through NamespaceID (required); AWSAccountID is only populated for
// cloudwatch sources since weftd sources don't have an AWS account. For
// pending rows: SchemaYAML is empty, LogGroupName is the app path, and
// Sample/Detection carry the auto-detected candidates from the most recent
// agent submission.
type LogGroup struct {
	gorm.Model
	NamespaceID uint      `gorm:"not null;uniqueIndex:idx_ns_app"`
	Namespace   Namespace `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	// CustomerID is denormalized from the owning namespace (migration
	// 054) so log_groups can be tenant-scoped directly. Read-only to
	// GORM (`->`): the BEFORE INSERT trigger fills it from namespace_id,
	// so GORM must omit it from INSERTs (a written zero-value would
	// dodge the trigger's NULL check and trip the customers FK).
	CustomerID   uint        `gorm:"->"`
	AWSAccountID *uint       // nullable; required for cloudwatch, nil for weftd
	AWSAccount   *AWSAccount `gorm:"foreignKey:AWSAccountID"`
	Service      string      `gorm:"column:app;not null;uniqueIndex:idx_ns_app"`
	// BucketID assigns this service to a bucket (migration 057). NULL =
	// the default bucket; a non-NULL value is a service moved to a
	// dedicated bucket. A service routes to exactly one bucket — the
	// unique(namespace_id, app) above makes that automatic.
	BucketID     *int64             `gorm:"column:bucket_id"`
	SourceType   LogGroupSourceType `gorm:"type:log_group_source_type;not null"`
	Status       LogGroupStatus     `gorm:"not null;default:configured"`
	LogGroupName string             // nullable: pending rows may not yet have a confirmed name
	Region       string             `gorm:"not null;default:us-east-2"`
	SchemaYAML   string             // nullable: pending rows don't yet have a schema
	PollInterval string             `gorm:"not null;default:30s"`
	Enabled      bool               `gorm:"not null;default:true"`
	LastPollAt   string
	LastToken    string
	Instance     string // hostname of the agent that last submitted a sample (pending only)
	Deployment   string `gorm:"column:deployment"` // fleet/scaling-unit (k8s Deployment, AWS ASG, etc.) — pending only
	Sample       string // JSON array of raw sample lines; cleared on configure
	Detection    string // JSON-encoded ingest.Detection; cleared on configure
	// SampleCollected is the accumulation progress (lines so far) while
	// Status == StatusSampling, so the UI/CLI can report
	// "<source> hasn't emitted enough lines to sample yet (N/50)".
	SampleCollected int
	// ResampleRequestedAt is set when a user asks for a fresh sample of
	// an already-configured source. The row stays StatusConfigured and
	// keeps ingesting on its current schema; the agent re-samples
	// concurrently and the fresh sample lands in Sample/Detection. The
	// coordinator clears this on the final (non-partial) resample submit.
	ResampleRequestedAt *time.Time
}

// AddLogGroup creates or updates a configured log group source. Ownership
// is keyed on (namespace_id, app). awsAccountID must be non-nil for
// cloudwatch sources and nil for weftd sources — the caller is trusted to
// match sourceType to account nullability. For weftd sources in the
// pending→configured transition, use ConfigureLogGroup instead; AddLogGroup
// is the CloudWatch add-source path where the schema is approved in one
// UI step.
func (s *Store) AddLogGroup(namespaceID uint, awsAccountID *uint, sourceType LogGroupSourceType, service, logGroupName, region, schemaYAML, pollInterval string) error {
	var lg LogGroup
	result := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		lg = LogGroup{
			NamespaceID:  namespaceID,
			AWSAccountID: awsAccountID,
			SourceType:   sourceType,
			Status:       StatusConfigured,
			Service:      service,
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
		"status":         StatusConfigured,
		"log_group_name": logGroupName,
		"region":         region,
		"schema_yaml":    schemaYAML,
		"poll_interval":  pollInterval,
		"sample":         nil,
		"detection":      nil,
	}).Error
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

// AllLogGroups returns configured log groups with customer/namespace info.
// Pending rows are excluded — callers that want them use
// ListPendingLogGroups. AWS-account fields are populated for cloudwatch
// rows and left empty for weftd rows via the LEFT JOIN.
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
		Where("log_groups.status = ?", StatusConfigured).
		Order("customers.name, namespaces.name, log_groups.app").
		Find(&results).Error
	return results, err
}

// ── Pending log groups ─────────────────────────────────────────
// A pending row holds the auto-detected schema candidate from a weftd
// sample. The agent keeps re-submitting until the source transitions to
// configured, at which point the sample/detection are cleared and
// schema_yaml is filled. (namespace_id, app) is unique across both
// states, so a sample arriving after configuration is a no-op — the
// configured row is preserved and the stale pending UPSERT is dropped.
// That invariant is what stops the old bug of a source appearing as
// BOTH pending and configured in the UI.

// UpsertPendingLogGroup records the most recent sample + detection from a
// weftd agent for (namespace_id, app). collected is the running line count;
// partial=true means the agent is still accumulating (a progress ping),
// partial=false is the final, complete sample.
//
// State transitions:
//   - No row: insert. partial → StatusSampling (still accumulating, not
//     yet configurable); final → StatusPending (ready for human review).
//   - StatusSampling/StatusPending row: refresh sample/detection/progress;
//     partial keeps it StatusSampling, final promotes to StatusPending.
//   - StatusConfigured row WITH a pending resample: the fresh sample lands
//     in sample/detection but the row stays configured and keeps ingesting
//     on its current schema. The final submit clears the resample flag,
//     making the new sample available for review.
//   - StatusConfigured row WITHOUT a resample: no-op. The schema is
//     approved and this sample is stale; preserving this invariant is what
//     stops a source appearing as both pending and configured.
func (s *Store) UpsertPendingLogGroup(namespaceID uint, service, instance, deployment, sampleJSON, detectionJSON string, collected int, partial bool) error {
	status := StatusPending
	if partial {
		status = StatusSampling
	}
	var lg LogGroup
	err := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		lg = LogGroup{
			NamespaceID:     namespaceID,
			Service:         service,
			Status:          status,
			SourceType:      SourceTypeWeftd,
			LogGroupName:    service,
			Region:          "local",
			PollInterval:    "0s",
			Enabled:         true,
			Instance:        instance,
			Deployment:      deployment,
			Sample:          sampleJSON,
			Detection:       detectionJSON,
			SampleCollected: collected,
		}
		return s.db.Create(&lg).Error
	}
	if err != nil {
		return err
	}
	if lg.Status == StatusConfigured {
		if lg.ResampleRequestedAt == nil {
			return nil // stale sample, schema already approved
		}
		// Resample in flight: refresh the fresh sample without
		// disturbing status/schema (ingestion continues). The final
		// submit clears the flag so the new sample surfaces for review.
		updates := map[string]any{
			"instance":         instance,
			"deployment":       deployment,
			"sample":           sampleJSON,
			"detection":        detectionJSON,
			"sample_collected": collected,
		}
		if !partial {
			updates["resample_requested_at"] = nil
		}
		return s.db.Model(&lg).Updates(updates).Error
	}
	return s.db.Model(&lg).Updates(map[string]any{
		"status":           status,
		"instance":         instance,
		"deployment":       deployment,
		"sample":           sampleJSON,
		"detection":        detectionJSON,
		"sample_collected": collected,
	}).Error
}

// FindLogGroup returns the single log_groups row for (customer, namespace,
// app) regardless of status, or nil if none exists. Used by the
// view-schema and resample endpoints, which need the row (and its
// namespace id / status / schema) before acting.
func (s *Store) FindLogGroup(customerID uint, namespace, app string) (*LogGroupWithAWSAccount, error) {
	fullApp := namespace + "/" + app
	var lg LogGroupWithAWSAccount
	err := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        customers.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN customers ON namespaces.customer_id = customers.id").
		Where("customers.id = ? AND namespaces.name = ? AND log_groups.app = ?",
			customerID, namespace, fullApp).
		First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lg, nil
}
