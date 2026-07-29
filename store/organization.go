package store

import (
	"time"

	"gorm.io/gorm"
)

// Organization is a weft tenant — what the code and the CLI used to call a
// Customer. It is the same row it always was, re-keyed onto the UUID identity
// the shared saaskit/orgs library uses, so weft and vimgym describe a team the
// same way.
//
// Two distinct names live here and they are not interchangeable:
//
//   - Name is the tenant *slug*. It is what gets stamped into every log row and
//     what the shard-side isolation predicate matches (`WHERE customer =
//     '<name>'`), so it is uniquely indexed and effectively immutable.
//   - DisplayName is the human-readable team name, freely editable, and load
//     bearing for nothing.
//
// The Stripe columns and SeatCount come from the orgs shape. weft keeps the
// substance of billing — tier, comp, expiry — in Subscription, which is richer
// than anything orgs models; these carry only the org-level Stripe identity.
type Organization struct {
	ID          string `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   gorm.DeletedAt `gorm:"index"`
	Name        string         `gorm:"uniqueIndex;not null"`
	DisplayName string         // human-readable org/team name (editable)

	StripeCustomerID     string
	StripeSubscriptionID string
	SeatCount            int `gorm:"not null;default:0"`

	Namespaces  []Namespace  `gorm:"foreignKey:OrgID;constraint:OnDelete:CASCADE"`
	AWSAccounts []AWSAccount `gorm:"foreignKey:OrgID;constraint:OnDelete:CASCADE"`
	AgentTokens []AgentToken `gorm:"foreignKey:OrgID;constraint:OnDelete:CASCADE"`
}

// TableName pins the table across the rename; gorm would otherwise pluralize
// the struct to "organizations" anyway, but saying so makes the mapping
// explicit rather than a naming coincidence.
func (Organization) TableName() string { return "organizations" }

// Namespace is a grouping of log sources owned by an org.
// e.g., "weft" (containing coordinator, shard, api sub-apps).
// Agent tokens and log ingestion reference namespaces via "namespace/subapp" app names.
type Namespace struct {
	gorm.Model
	OrgID string `gorm:"type:uuid;not null;uniqueIndex:idx_ns_org_name"`
	Name  string `gorm:"not null;uniqueIndex:idx_ns_org_name"`
}

// FindOrCreateOrg returns the org with this slug, creating it if needed.
func (s *Store) FindOrCreateOrg(name string) (*Organization, error) {
	var c Organization
	result := s.db.Where("name = ?", name).FirstOrCreate(&c, Organization{Name: name})
	return &c, result.Error
}

// GetOrgByID returns an org by primary key.
func (s *Store) GetOrgByID(id string) (*Organization, error) {
	var c Organization
	if err := s.db.Where("id = ?", id).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// GetOrgByName returns an org by slug.
func (s *Store) GetOrgByName(name string) (*Organization, error) {
	var c Organization
	if err := s.db.Where("name = ?", name).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// UpdateOrgDisplayName sets the human-readable team/org name.
func (s *Store) UpdateOrgDisplayName(name, displayName string) error {
	return s.db.Model(&Organization{}).Where("name = ?", name).Update("display_name", displayName).Error
}

// --- Namespaces ---

// FindOrCreateNamespace returns the namespace, creating if needed.
func (s *Store) FindOrCreateNamespace(orgID string, name string) (*Namespace, error) {
	var ns Namespace
	result := s.scoped(orgID).Where("name = ?", name).FirstOrCreate(&ns, Namespace{OrgID: orgID, Name: name})
	return &ns, result.Error
}

// GetNamespace looks up a namespace by (org_id, name) without creating
// one if absent. Callers validating user-supplied namespace names (e.g.
// AddSource) should use this so a typo doesn't silently spawn a new
// namespace under the org.
func (s *Store) GetNamespace(orgID string, name string) (*Namespace, error) {
	var ns Namespace
	if err := s.scoped(orgID).Where("name = ?", name).First(&ns).Error; err != nil {
		return nil, err
	}
	return &ns, nil
}

// ListNamespaces returns all namespaces for an org.
func (s *Store) ListNamespaces(orgID string) ([]Namespace, error) {
	var nss []Namespace
	err := s.scoped(orgID).Order("name").Find(&nss).Error
	return nss, err
}
