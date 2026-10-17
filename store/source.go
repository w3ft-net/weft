package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// LogGroupSourceType identifies where a log_groups row's rows come from.
// weftd:   weftd agents push rows via gRPC IngestStream; no polling.
// kinesis: customer-owned Kinesis Data Firehose stream pushes to
//
//	POST /api/ingest/cloudwatch; no polling, no cross-account role
//	registration on our side (see cloudwatch-kinesis-subscription.md).
//
// The enum also still carries a 'cloudwatch' value at the Postgres level
// (poll-based ingestion, retired; migration 077) — Postgres can't drop an
// enum value without recreating the type, so it stays as a legacy value
// nothing writes anymore rather than a case any Go code branches on.
type LogGroupSourceType string

const (
	SourceTypeWeftd LogGroupSourceType = "weftd"
	// SourceTypeDirect is a raw-push producer with no sampling handshake:
	// Heroku log drains, custom POSTs to /api/ingest, bare IngestStream
	// clients. The coordinator auto-creates these on first-seen ingest
	// (EnsureServiceLogGroup) rather than via GetSourceStatus/SubmitSample.
	SourceTypeDirect LogGroupSourceType = "direct"
	// SourceTypeKinesis is push-based CloudWatch ingestion via a
	// customer-owned Firehose delivery stream
	// (docs/plans/cloudwatch-kinesis-subscription.md).
	SourceTypeKinesis LogGroupSourceType = "kinesis"
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

// SampleTarget is the line count a full sample carries unless a resample
// asked for more (LogGroup.Target). The agent owns the real accumulation;
// this is the canonical value the coordinator and UIs use to report progress
// ("N/target").
const SampleTarget = 50

// MaxSampleTarget bounds a resample's requested size: the sample is stored on
// the log_groups row and handed to the classifier and the wizard whole.
const MaxSampleTarget = 2000

// ErrBadSampleSize is RequestResample's answer for a size outside
// [SampleTarget, MaxSampleTarget]; a resample never asks for fewer lines than
// the first sample had.
var ErrBadSampleSize = fmt.Errorf("sample size must be between %d and %d lines", SampleTarget, MaxSampleTarget)

const (
	// StatusSampling: the agent has announced the source but is still
	// accumulating toward a full sample. Not yet configurable.
	StatusSampling   LogGroupStatus = "sampling"
	StatusPending    LogGroupStatus = "pending"
	StatusConfigured LogGroupStatus = "configured"
)

// LogGroup represents a log source — pending or configured. Ownership is
// pinned through NamespaceID (required). For pending rows: SchemaYAML is
// empty, LogGroupName is the app path, and Sample/Detection carry the
// auto-detected candidates from the most recent agent submission.
type LogGroup struct {
	gorm.Model
	NamespaceID uint      `gorm:"not null;uniqueIndex:idx_ns_app"`
	Namespace   Namespace `gorm:"foreignKey:NamespaceID;constraint:OnDelete:CASCADE"`
	// OrgID is denormalized from the owning namespace (migration
	// 054) so log_groups can be tenant-scoped directly. Read-only to
	// GORM (`->`): the BEFORE INSERT trigger fills it from namespace_id,
	// so GORM must omit it from INSERTs (a written zero-value would
	// dodge the trigger's NULL check and trip the customers FK).
	OrgID   OrgID  `gorm:"->"`
	Service string `gorm:"column:app;not null;uniqueIndex:idx_ns_app"`
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
	Enabled      bool               `gorm:"not null;default:true"`
	Instance     string             // hostname of the agent that last submitted a sample (pending only)
	Deployment   string             `gorm:"column:deployment"` // fleet/scaling-unit (k8s Deployment, AWS ASG, etc.) — pending only
	Sample       string             // JSON array of raw sample lines; cleared on configure
	Detection    string             // JSON-encoded ingest.Detection; cleared on configure
	// SampleCollected is the accumulation progress (lines so far) while
	// Status == StatusSampling, so the UI/CLI can report
	// "<source> hasn't emitted enough lines to sample yet (N/50)".
	SampleCollected int
	// SampleUpdatedAt is when the sample last changed (server clock): the
	// latest lines arriving, or a resample request resetting it. A
	// still-filling sample shows how live it is from this.
	SampleUpdatedAt *time.Time
	// ResampleRequestedAt is set when a user asks for a fresh sample of
	// an already-configured source. The row stays StatusConfigured and
	// keeps ingesting on its current schema; the agent re-samples
	// concurrently and the fresh sample lands in Sample/Detection. The
	// coordinator clears this on the final (non-partial) resample submit.
	ResampleRequestedAt *time.Time
	// SampleSize is the line count the current sample is collecting toward,
	// when a resample asked for more than SampleTarget (a field that appears
	// on only some requests needs a bigger haystack). Zero means the default.
	SampleSize int `gorm:"column:sample_target"`
}

// Target is the line count a full sample of this source carries.
func (lg LogGroup) Target() int {
	if lg.SampleSize > 0 {
		return lg.SampleSize
	}
	return SampleTarget
}

// AddLogGroup creates or updates a configured log group source. Ownership
// is keyed on (namespace_id, app). For weftd sources in the
// pending→configured transition, use ConfigureLogGroup instead.
func (s *Store) AddLogGroup(ctx context.Context, namespaceID uint, sourceType LogGroupSourceType, service, logGroupName, region, schemaYAML string) error {
	var lg LogGroup
	result := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		lg = LogGroup{
			NamespaceID:  namespaceID,
			SourceType:   sourceType,
			Status:       StatusConfigured,
			Service:      service,
			LogGroupName: logGroupName,
			Region:       region,
			SchemaYAML:   schemaYAML,
			Enabled:      true,
		}
		if err := s.db.Create(&lg).Error; err != nil {
			return err
		}
		s.invalidateSchemaCache(ctx, namespaceID, service)
		return nil
	}
	if err := s.db.Model(&lg).Updates(map[string]any{
		"source_type":    sourceType,
		"status":         StatusConfigured,
		"log_group_name": logGroupName,
		"region":         region,
		"schema_yaml":    schemaYAML,
		"sample":         nil,
		"detection":      nil,
	}).Error; err != nil {
		return err
	}
	s.invalidateSchemaCache(ctx, namespaceID, service)
	return nil
}

// CreatePendingKinesisSource registers a namespace-scoped mapping from an
// AWS CloudWatch log group name to a weft service for the Firehose push
// path (source_type=kinesis) — CloudWatchAdapter.FindLogGroupByAWSName
// needs this row to exist before it will route any event to a service.
// Unlike AddLogGroup (poll mode's one-shot "commit with schema in hand"),
// there's no schema yet and no cross-account role to assume: the row
// starts StatusPending, exactly like a fresh weftd source, and is
// finished via the existing pending→configure flow (ConfigureLogGroup,
// unmodified) once a human has something to configure against — the
// ingest path accumulates a sample from the first live-pushed rows for
// exactly that purpose (see enrichRows in api/adapter.go).
//
// Idempotent on (namespaceID, service): a second call is a no-op rather
// than an error, so `weft source add` is safe to re-run. Does not
// resurrect or re-map an already-pending-or-configured row to a
// different log group — callers that need to change the mapping should
// remove and re-add.
func (s *Store) CreatePendingKinesisSource(namespaceID uint, service, logGroupName, region string) error {
	var existing LogGroup
	result := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&existing)
	if result.Error == nil {
		return nil
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return result.Error
	}
	lg := LogGroup{
		NamespaceID:  namespaceID,
		SourceType:   SourceTypeKinesis,
		Status:       StatusPending,
		Service:      service,
		LogGroupName: logGroupName,
		Region:       region,
		Enabled:      true,
	}
	return s.db.Create(&lg).Error
}

// ConfigureLogGroup lands a schema on an existing pending-or-configured
// row — the weftd onboarding approval step (pending→configured) and the
// resample-schema-swap path (a resample leaves the row configured but
// with a fresh sample; comment on api/sources.go's caller has the full
// story). Both are the same write: a customer or agent producing a new
// schema_yaml for a service the coordinator already knows about.
func (s *Store) ConfigureLogGroup(ctx context.Context, namespaceID uint, service, schemaYAML string) error {
	res := s.db.Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ? AND status IN ?",
			namespaceID, service, []LogGroupStatus{StatusPending, StatusConfigured}).
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
		// The row disappeared (or left pending/configured) between the
		// caller's own lookup and this UPDATE — a parallel configure won
		// the race, or the source got removed. Callers translate this
		// into a 409, same as before this was a dedicated method.
		return gorm.ErrRecordNotFound
	}
	s.invalidateSchemaCache(ctx, namespaceID, service)
	return nil
}

// invalidateSchemaCache resolves namespaceID to the (orgID, namespace
// name) tuple FindLogGroupSchema's cache keys off of, then evicts this
// service's cached entry. A separate small lookup rather than requiring
// every AddLogGroup/ConfigureLogGroup caller to thread org/namespace
// name through — schema writes are rare (customer-driven, not per log
// line), so the extra indexed-PK query here is not a hot path.
//
// Namespace is tenant-scoped (store.go's enroll list), so this must go
// through crossTenant() rather than s.db directly — a plain s.db.First
// here has no org in its context and silently fails closed with
// ErrNoOrgInContext (tenant.go's scope callback), which would make this
// function return early without ever calling schemaCache.invalidate.
// Legitimate here the same way ValidateAgentToken's token-by-hash
// lookup is: resolving the tenant identity is the query's very purpose,
// so it can't itself be scoped by that identity.
//
// Best-effort: SchemaCache.invalidate is nil-safe and never surfaces an
// error to the write path, matching the cache's overall philosophy —
// worst case a missed invalidation is caught by the cache's own TTL.
func (s *Store) invalidateSchemaCache(ctx context.Context, namespaceID uint, service string) {
	if s.schemaCache == nil {
		return
	}
	var ns Namespace
	if err := s.crossTenant().First(&ns, namespaceID).Error; err != nil {
		return
	}
	s.schemaCache.invalidate(ctx, ns.OrgID, ns.Name, service)
}

// FindLogGroupSchema resolves a service's configured schema for ingest-
// time enrichment (api.enrichRow/enrichRows, SyslogServer), cache-aside
// against the optional Valkey schema cache — see schema_cache.go.
// ok=false means "no configured schema" (never onboarded, still
// pending, or schema_yaml empty), not an error; callers skip enrichment
// and leave the row Raw-only, exactly the pre-cache behavior.
func (s *Store) FindLogGroupSchema(ctx context.Context, orgID OrgID, namespace string, service string) (schemaYAML string, ok bool, err error) {
	if entry, hit := s.schemaCache.get(ctx, orgID, namespace, service); hit {
		return entry.SchemaYAML, entry.Configured, nil
	}
	lg, err := s.FindLogGroup(orgID, namespace, service)
	if err != nil {
		return "", false, err
	}
	var entry cachedSchemaEntry
	if lg != nil && lg.Status == StatusConfigured && lg.SchemaYAML != "" {
		entry = cachedSchemaEntry{Configured: true, SchemaYAML: lg.SchemaYAML}
	}
	s.schemaCache.set(ctx, orgID, namespace, service, &entry)
	return entry.SchemaYAML, entry.Configured, nil
}

// FindLogGroupByAWSName resolves an AWS CloudWatch log group name to its
// configured weft service, scoped to (orgID, namespace) —
// CloudWatchAdapter's per-event routing step (envelope.logGroup →
// LogRow.Service; see docs/plans/cloudwatch-kinesis-subscription.md,
// "Adapter-level logGroup → service resolution"). ok=false covers "no
// log_groups row with this AWS name in this namespace," not an error;
// the caller drops the event rather than routing it to service="". A
// plain indexed Postgres lookup, not cached — no evidence yet that a
// Firehose batch's per-event reverse lookup needs the same Valkey
// cache-aside treatment as FindLogGroupSchema; adapter_cloudwatch.go
// memoizes this per-request instead, which covers the common case of
// one log group per batch.
func (s *Store) FindLogGroupByAWSName(orgID OrgID, namespace string, logGroupName string) (service string, ok bool, err error) {
	var lg LogGroup
	err = s.db.
		Table("log_groups").
		Select("log_groups.app").
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id").
		Where("organizations.id = ? AND namespaces.name = ? AND log_groups.log_group_name = ?",
			orgID, namespace, logGroupName).
		First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return lg.Service, true, nil
}

// LogGroupJoined is a joined result for the list paths — a LogGroup plus
// the customer/namespace names pulled in via the join, so callers don't
// need a second query to resolve ownership.
type LogGroupJoined struct {
	LogGroup
	CustomerName  string
	NamespaceName string
}

// AllLogGroups returns configured log groups with customer/namespace info.
// Pending rows are excluded — callers that want them use
// ListPendingLogGroups.
func (s *Store) AllLogGroups() ([]LogGroupJoined, error) {
	return s.allLogGroups(StatusConfigured)
}

// AllLogGroupsAnyStatus returns every source regardless of status — including
// pending and still-sampling rows. Backs the source-list view so it surfaces
// (and can remove) sources that haven't been configured yet.
func (s *Store) AllLogGroupsAnyStatus() ([]LogGroupJoined, error) {
	return s.allLogGroups()
}

// allLogGroups runs the shared join. With no statuses it returns every row;
// otherwise it filters to the given ones.
func (s *Store) allLogGroups(statuses ...LogGroupStatus) ([]LogGroupJoined, error) {
	var results []LogGroupJoined
	q := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        organizations.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id")
	if len(statuses) > 0 {
		q = q.Where("log_groups.status IN ?", statuses)
	}
	err := q.Order("organizations.name, namespaces.name, log_groups.app").Find(&results).Error
	return results, err
}

// AwaitingSchema scopes a log_groups query to sources a human still has to
// give a schema: weftd/kinesis rows that are sampling or pending, plus
// configured rows that are collecting or holding a sample for review:
//   - raw-push rows that are live but schemaless and have collected lines
//     (or had their sample reset by a resample request) — their sample
//     accumulates while they keep ingesting;
//   - a configured row that has a schema and a resample in flight (flag
//     set), or a fresh sample left by a finished resample. Configuring clears
//     both, so such a row is one the user asked to re-sample and has not yet
//     re-configured. (A schemaless row's flag only means "still collecting",
//     which the rule above already covers.)
func AwaitingSchema(q *gorm.DB) *gorm.DB {
	return q.Where(`log_groups.status IN ? OR (log_groups.status = ? AND (
		(COALESCE(log_groups.schema_yaml, '') = '' AND (log_groups.sample_collected > 0 OR log_groups.sample_updated_at IS NOT NULL))
		OR (COALESCE(log_groups.schema_yaml, '') <> '' AND (
			log_groups.resample_requested_at IS NOT NULL
			OR COALESCE(log_groups.sample, '') NOT IN ('', '[]', 'null')))))`,
		[]LogGroupStatus{StatusSampling, StatusPending}, StatusConfigured)
}

// CountPendingLogGroups counts an org's sources awaiting a schema — the same
// set the pending list returns, without loading the rows (and their
// sample/detection blobs).
func (s *Store) CountPendingLogGroups(orgID OrgID) (int, error) {
	var n int64
	err := s.db.
		Table("log_groups").
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Where("namespaces.org_id = ?", orgID).
		Scopes(AwaitingSchema).
		Count(&n).Error
	return int(n), err
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
//
// EnsureServiceLogGroup idempotently records that (namespaceID, service)
// exists as a directly-pushed source in the default bucket (bucket_id
// NULL → default via ServicesForBucket's COALESCE). It's the auto-onboard
// path for raw producers that never sample: the row is the canonical
// (namespace, service) mapping shards inherit at christen, created
// already-configured (ingest never waits on a schema — raw lines are
// queryable as is). It is created with a resample requested, which is how a
// schemaless source gets its sample: the ingest path accumulates live lines
// into the row (accumulatePendingSamples) while it keeps ingesting, and the
// full sample surfaces it for schema review (AwaitingSchema). No-op if a
// log_group for (namespaceID, service) already exists, so a
// weftd/cloudwatch source that later also pushes directly isn't clobbered.
func (s *Store) EnsureServiceLogGroup(namespaceID uint, service string) error {
	var lg LogGroup
	err := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&lg).Error
	if err == nil {
		return nil // already mapped (any source type) — leave it
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	now := time.Now()
	return s.db.Create(&LogGroup{
		NamespaceID:         namespaceID,
		Service:             service,
		Status:              StatusConfigured,
		SourceType:          SourceTypeDirect,
		LogGroupName:        service,
		Region:              "local",
		Enabled:             true,
		ResampleRequestedAt: &now,
	}).Error
}

func (s *Store) UpsertPendingLogGroup(namespaceID uint, service, instance, deployment, sampleJSON, detectionJSON string, collected int, partial bool) error {
	now := time.Now()
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
			Enabled:         true,
			Instance:        instance,
			Deployment:      deployment,
			Sample:          sampleJSON,
			Detection:       detectionJSON,
			SampleCollected: collected,
			SampleUpdatedAt: &now,
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
			"instance":          instance,
			"deployment":        deployment,
			"sample":            sampleJSON,
			"detection":         detectionJSON,
			"sample_collected":  collected,
			"sample_updated_at": now,
		}
		if !partial {
			updates["resample_requested_at"] = nil
		}
		return s.db.Model(&lg).Updates(updates).Error
	}
	updates := map[string]any{
		"status":            status,
		"instance":          instance,
		"deployment":        deployment,
		"sample":            sampleJSON,
		"detection":         detectionJSON,
		"sample_collected":  collected,
		"sample_updated_at": now,
	}
	if !partial {
		updates["resample_requested_at"] = nil // a final sample ends any resample
	}
	return s.db.Model(&lg).Updates(updates).Error
}

// ErrAlreadySampling is RequestResample's answer for a source still filling
// its first sample: it is already collecting, so there is nothing to restart.
var ErrAlreadySampling = errors.New("a sample is already being collected")

// RequestResample asks for a fresh sample of (namespaceID, service), for when
// the lines sampled so far (process startup, say) don't show the values a
// schema needs to extract. It works on pending and configured sources:
//
//   - A pending source goes back to StatusSampling with its sample cleared.
//   - A configured source keeps ingesting on its current schema.
//
// Either way ResampleRequestedAt is set, which is what a waiting weftd agent
// polls for. Kinesis and direct sources have no agent to sample for them: the
// ingest path fills their sample from live rows (accumulateSample) and stops
// at SampleTarget, so their old sample and count are reset here too.
//
// size is the number of lines to collect (0 means SampleTarget).
//
// Returns gorm.ErrRecordNotFound for a missing source, ErrAlreadySampling
// for one that is already collecting a sample (its first, or a resample), and
// ErrBadSampleSize for a size out of range.
func (s *Store) RequestResample(namespaceID uint, service string, size int) error {
	if size == 0 {
		size = SampleTarget
	}
	if size < SampleTarget || size > MaxSampleTarget {
		return ErrBadSampleSize
	}
	var lg LogGroup
	err := s.db.Where("namespace_id = ? AND app = ?", namespaceID, service).First(&lg).Error
	if err != nil {
		return err
	}
	// Already collecting: still on the first sample, or a resample is in
	// flight (flag set; a direct source stays "configured" throughout).
	if lg.Status == StatusSampling || (lg.Status == StatusConfigured && lg.ResampleRequestedAt != nil) {
		return ErrAlreadySampling
	}
	updates := map[string]any{"resample_requested_at": time.Now(), "sample_target": size}
	if lg.Status == StatusPending {
		updates["status"] = StatusSampling
	}
	if lg.Status == StatusPending || lg.SourceType == SourceTypeKinesis || lg.SourceType == SourceTypeDirect {
		updates["sample"] = nil
		updates["detection"] = nil
		updates["sample_collected"] = 0
		// Stamped, not cleared: "the sample changed (was reset) now". This is
		// also what keeps a schemaless direct source listed (AwaitingSchema)
		// while it has collected nothing yet.
		updates["sample_updated_at"] = time.Now()
	}
	return s.db.Model(&lg).Updates(updates).Error
}

// UpdateLogGroupDetection writes a freshly (LLM-)classified Detection for
// (namespaceID, service) without touching sample/collected/status —
// called from the background goroutine ClassifyAsync's callers kick off
// after the synchronous sample-submit request has already returned.
//
// sampleJSON is the sample the classification was computed for. The write only
// applies while that is still the row's sample, so a late result never
// overwrites the detection of a newer sample, and is a harmless no-op if the
// row was deleted or its sample was never stored (a configured source with no
// resample in flight ignores stray samples). Status is deliberately not part of
// the test: a configured source whose resample just completed is waiting for
// exactly this detection.
func (s *Store) UpdateLogGroupDetection(namespaceID uint, service, sampleJSON, detectionJSON string) error {
	return s.db.Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ? AND sample = ?", namespaceID, service, sampleJSON).
		Update("detection", detectionJSON).Error
}

// FindLogGroup returns the single log_groups row for (customer, namespace,
// service) regardless of status, or nil if none exists. Used by the
// view-schema and resample endpoints, which need the row (and its
// namespace id / status / schema) before acting. log_groups.app holds
// the bare service (migration 060), scoped to the namespace by the join
// — so the match is on the bare service, not a "namespace/service"
// composite.
func (s *Store) FindLogGroup(orgID OrgID, namespace, service string) (*LogGroupJoined, error) {
	var lg LogGroupJoined
	err := s.db.
		Table("log_groups").
		Select(`log_groups.*,
		        organizations.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id").
		Where("organizations.id = ? AND namespaces.name = ? AND log_groups.app = ?",
			orgID, namespace, service).
		First(&lg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lg, nil
}
