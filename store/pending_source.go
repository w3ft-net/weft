package store

import (
	"time"

	"gorm.io/gorm"
)

// PendingSource holds a log source that has submitted samples but
// has not yet been configured with a schema by a human reviewer.
type PendingSource struct {
	CustomerID uint      `gorm:"not null;uniqueIndex:idx_pending_customer_ns_app"`
	Customer   Customer  `gorm:"foreignKey:CustomerID"`
	Namespace  string    `gorm:"not null;uniqueIndex:idx_pending_customer_ns_app"`
	App        string    `gorm:"not null;uniqueIndex:idx_pending_customer_ns_app"`
	Instance   string    // hostname of the agent that submitted the sample
	HostGroup  string    // logical group (e.g. "coordinator", "shard")
	Sample     string    `gorm:"type:jsonb;not null"` // JSON array of raw sample lines
	Detection  string    `gorm:"type:jsonb;not null"` // JSON-encoded ingest.Detection result
	CreatedAt  time.Time `gorm:"autoCreateTime"`
}

// UpsertPendingSource creates or updates a pending source sample.
// Idempotent: a second submission for the same (customer, namespace, app) overwrites.
func (s *Store) UpsertPendingSource(customerID uint, namespace, app, instance, hostGroup, sampleJSON, detectionJSON string) error {
	var ps PendingSource
	result := s.db.Where("customer_id = ? AND namespace = ? AND app = ?", customerID, namespace, app).First(&ps)
	if result.Error == gorm.ErrRecordNotFound {
		ps = PendingSource{
			CustomerID: customerID,
			Namespace:  namespace,
			App:        app,
			Instance:   instance,
			HostGroup:  hostGroup,
			Sample:     sampleJSON,
			Detection:  detectionJSON,
		}
		return s.db.Create(&ps).Error
	}
	if result.Error != nil {
		return result.Error
	}
	return s.db.Model(&ps).Updates(PendingSource{
		Instance:  instance,
		HostGroup: hostGroup,
		Sample:    sampleJSON,
		Detection: detectionJSON,
	}).Error
}

// GetPendingSource returns a single pending source or nil.
func (s *Store) GetPendingSource(customerID uint, namespace, app string) (*PendingSource, error) {
	var ps PendingSource
	result := s.db.Where("customer_id = ? AND namespace = ? AND app = ?", customerID, namespace, app).First(&ps)
	if result.Error == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &ps, nil
}

// ListPendingSources returns all pending sources for a customer, optionally filtered by namespace.
func (s *Store) ListPendingSources(customerID uint, namespace string) ([]PendingSource, error) {
	var sources []PendingSource
	q := s.db.Where("customer_id = ?", customerID)
	if namespace != "" {
		q = q.Where("namespace = ?", namespace)
	}
	err := q.Order("namespace, app").Find(&sources).Error
	return sources, err
}

// DeletePendingSource removes a pending source (after it has been configured).
func (s *Store) DeletePendingSource(customerID uint, namespace, app string) error {
	return s.db.Where("customer_id = ? AND namespace = ? AND app = ?", customerID, namespace, app).
		Delete(&PendingSource{}).Error
}

// SourceIsConfigured checks whether a fully-configured log source exists
// for the given (customer, namespace/app) combination.
func (s *Store) SourceIsConfigured(customerName, namespace, app string) (bool, string, error) {
	// The log_groups table stores the full "namespace/app" as the app field.
	fullApp := namespace + "/" + app
	var lg LogGroup
	result := s.db.
		Joins("JOIN accounts ON log_groups.account_id = accounts.id").
		Joins("JOIN customers ON accounts.customer_id = customers.id").
		Where("customers.name = ? AND log_groups.app = ?", customerName, fullApp).
		First(&lg)
	if result.Error == gorm.ErrRecordNotFound {
		return false, "", nil
	}
	if result.Error != nil {
		return false, "", result.Error
	}
	return true, lg.SchemaYAML, nil
}
