package store

import (
	"errors"
	"strings"
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
	NamespaceID  uint               `gorm:"not null;uniqueIndex:idx_ns_app"`
	Namespace    Namespace          `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	// CustomerID is denormalized from the owning namespace (migration
	// 054) so log_groups can be tenant-scoped directly. Read-only to
	// GORM (`->`): the BEFORE INSERT trigger fills it from namespace_id,
	// so GORM must omit it from INSERTs (a written zero-value would
	// dodge the trigger's NULL check and trip the customers FK).
	CustomerID   uint               `gorm:"->"`
	AWSAccountID *uint              // nullable; required for cloudwatch, nil for weftd
	AWSAccount   *AWSAccount        `gorm:"foreignKey:AWSAccountID"`
	App          string             `gorm:"not null;uniqueIndex:idx_ns_app"`
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

// AddLogGroup creates or updates a configured log group source. Ownership
// is keyed on (namespace_id, app). awsAccountID must be non-nil for
// cloudwatch sources and nil for weftd sources — the caller is trusted to
// match sourceType to account nullability. For weftd sources in the
// pending→configured transition, use ConfigureLogGroup instead; AddLogGroup
// is the CloudWatch add-source path where the schema is approved in one
// UI step.
func (s *Store) AddLogGroup(namespaceID uint, awsAccountID *uint, sourceType LogGroupSourceType, app, logGroupName, region, schemaYAML, pollInterval string) error {
	var lg LogGroup
	result := s.db.Where("namespace_id = ? AND app = ?", namespaceID, app).First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		lg = LogGroup{
			NamespaceID:  namespaceID,
			AWSAccountID: awsAccountID,
			SourceType:   sourceType,
			Status:       StatusConfigured,
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
		"status":         StatusConfigured,
		"log_group_name": logGroupName,
		"region":         region,
		"schema_yaml":    schemaYAML,
		"poll_interval":  pollInterval,
		"sample":         nil,
		"detection":      nil,
	}).Error
}

// CustomerAppNames returns the distinct app names visible to the
// customer — the union of every enabled, configured log_groups row
// in any namespace they own. Used by the glob expander to enumerate
// pseudofile candidates for `cat /log/<ns>/*.log`-style patterns.
func (s *Store) CustomerAppNames(customerID uint) ([]string, error) {
	var names []string
	err := s.db.Model(&LogGroup{}).
		Joins("JOIN namespaces ON namespaces.id = log_groups.namespace_id").
		Where("namespaces.customer_id = ? AND log_groups.enabled = ? AND log_groups.status = ?",
			customerID, true, StatusConfigured).
		Distinct().Pluck("log_groups.app", &names).Error
	return names, err
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

// EnabledLogGroups returns enabled, configured cloudwatch log groups with
// customer/account info. Weftd sources are excluded because the poller
// doesn't drive them; pending rows are excluded because they have no
// schema to poll against.
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
		Where("log_groups.enabled = ? AND log_groups.source_type = ? AND log_groups.status = ?",
			true, SourceTypeCloudWatch, StatusConfigured).
		Find(&results).Error
	return results, err
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

// SourceIsConfigured reports whether a configured log_groups row exists
// for (customerName, namespace, app) and returns its SchemaYAML. Pending
// rows return (false, "", nil). Used by handleAgentSourceStatus to tell
// weftd "you may start streaming" vs "wait for human review".
func (s *Store) SourceIsConfigured(customerName, namespace, app string) (bool, string, error) {
	fullApp := namespace + "/" + app
	var lg LogGroup
	result := s.db.
		Table("log_groups").
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN customers ON namespaces.customer_id = customers.id").
		Where("customers.name = ? AND log_groups.app = ? AND log_groups.status = ?",
			customerName, fullApp, StatusConfigured).
		First(&lg)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return false, "", nil
	}
	if result.Error != nil {
		return false, "", result.Error
	}
	return true, lg.SchemaYAML, nil
}

// UpdatePollState saves the last poll timestamp and pagination token for
// the given log_groups row id. Keyed by id rather than app since app is
// only unique within a namespace.
func (s *Store) UpdatePollState(id uint, lastPollAt, lastToken string) error {
	return s.db.Model(&LogGroup{}).Where("id = ?", id).
		Updates(map[string]any{"last_poll_at": lastPollAt, "last_token": lastToken}).Error
}

// AppNames returns distinct app names from enabled, configured log groups.
// Pending rows are deliberately excluded — they have no schema and no
// routable data.
func (s *Store) AppNames() ([]string, error) {
	var names []string
	err := s.db.Model(&LogGroup{}).
		Where("enabled = ? AND status = ?", true, StatusConfigured).
		Distinct().Pluck("app", &names).Error
	return names, err
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
func (s *Store) UpsertPendingLogGroup(namespaceID uint, app, instance, deployment, sampleJSON, detectionJSON string, collected int, partial bool) error {
	status := StatusPending
	if partial {
		status = StatusSampling
	}
	var lg LogGroup
	err := s.db.Where("namespace_id = ? AND app = ?", namespaceID, app).First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		lg = LogGroup{
			NamespaceID:     namespaceID,
			App:             app,
			Status:          status,
			SourceType:      SourceTypeWeftd,
			LogGroupName:    app,
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

// GetPendingLogGroup returns the pending row for (namespace_id, app) or
// nil if none exists (including the case where the row is already
// configured — callers that want the row regardless should read the
// table directly).
func (s *Store) GetPendingLogGroup(namespaceID uint, app string) (*LogGroup, error) {
	var lg LogGroup
	err := s.db.Where("namespace_id = ? AND app = ? AND status IN ?",
		namespaceID, app, []LogGroupStatus{StatusSampling, StatusPending}).First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lg, nil
}

// ListPendingLogGroups returns all pending rows for the customer,
// optionally filtered to a single namespace. Uses LogGroupWithAWSAccount
// for consistency with the configured-list path — AWS fields are always
// empty for pending rows (weftd-only today).
func (s *Store) ListPendingLogGroups(customerID uint, namespace string) ([]LogGroupWithAWSAccount, error) {
	var results []LogGroupWithAWSAccount
	q := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        customers.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN customers ON namespaces.customer_id = customers.id").
		Where("log_groups.status IN ? AND customers.id = ?",
			[]LogGroupStatus{StatusSampling, StatusPending}, customerID)
	if namespace != "" {
		q = q.Where("namespaces.name = ?", namespace)
	}
	err := q.Order("namespaces.name, log_groups.app").Find(&results).Error
	return results, err
}

// ConfigureLogGroup applies a schema to a source and marks it configured.
// It serves two cases:
//   - a pending source being configured for the first time, and
//   - an already-configured source whose schema is being swapped after a
//     resample (the row stayed configured and kept ingesting; this is the
//     non-disruptive switchover to the new schema).
//
// Sample/detection and any resample flag/progress are cleared; instance
// and deployment are preserved as provenance. A StatusSampling row is not
// matched — it has no full sample yet, so there's nothing to configure.
// Returns gorm.ErrRecordNotFound if no eligible row matches.
func (s *Store) ConfigureLogGroup(namespaceID uint, app, schemaYAML string) error {
	res := s.db.Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ? AND status IN ?",
			namespaceID, app, []LogGroupStatus{StatusPending, StatusConfigured}).
		Updates(map[string]any{
			"status":                StatusConfigured,
			"schema_yaml":           schemaYAML,
			"sample":                nil,
			"detection":             nil,
			"sample_collected":      0,
			"resample_requested_at": nil,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// RequestResample marks a configured source for a fresh sample. The row
// stays configured and keeps ingesting on its current schema; the agent
// picks up the flag on its next status poll, re-samples concurrently with
// its tailer, and resubmits. Returns gorm.ErrRecordNotFound if no
// configured row matches (only configured sources can be resampled — a
// pending/sampling source is already producing a fresh sample).
func (s *Store) RequestResample(namespaceID uint, app string) error {
	now := time.Now()
	res := s.db.Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ? AND status = ?",
			namespaceID, app, StatusConfigured).
		Update("resample_requested_at", now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
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
