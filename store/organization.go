package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gorm.io/gorm"
)

// OrgID is an organization's primary key: a bigint identity. saaskit's orgs
// package treats org ids as opaque strings, so ids cross into it via String()
// and come back via ParseOrgID.
type OrgID int64

// String is the decimal form saaskit's string-typed org ids carry.
func (id OrgID) String() string { return strconv.FormatInt(int64(id), 10) }

// ParseOrgID parses the decimal form produced by String.
func ParseOrgID(s string) (OrgID, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid org id %q", s)
	}
	return OrgID(n), nil
}

// Organization is a weft tenant — what the code and the CLI used to call a
// Customer. Its primary key is a bigint identity (OrgID). The shared
// saaskit/orgs library treats org ids as opaque strings, so weft uses it
// unchanged (its SchemaSQLBigintOrgID variant) and ids cross into it as
// decimal strings.
//
// The id and the names are not interchangeable:
//
//   - ID is the tenant's identity. It is what gets stamped into every log row
//     (customer_id) and what the shard-side isolation predicate matches
//     (`WHERE customer_id = ?`). It never changes and is never user-visible.
//   - Name is the tenant *slug*: what users type (X-Active-Org, the CLI) and
//     what the UI shows. It is uniquely indexed and resolved to an ID once,
//     at the session boundary.
//   - DisplayName is the human-readable team name, freely editable, and load
//     bearing for nothing.
//
// The Stripe columns and SeatCount come from the orgs shape. weft keeps the
// substance of billing — tier, comp, expiry — in Subscription, which is richer
// than anything orgs models; these carry only the org-level Stripe identity.
type Organization struct {
	ID          OrgID `gorm:"primaryKey"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   gorm.DeletedAt `gorm:"index"`
	Name        string         `gorm:"uniqueIndex;not null"`
	DisplayName string         // human-readable org/team name (editable)

	StripeCustomerID     string
	StripeSubscriptionID string
	SeatCount            int `gorm:"not null;default:0"`

	Namespaces  []Namespace  `gorm:"foreignKey:OrgID;constraint:OnDelete:CASCADE"`
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
	OrgID OrgID  `gorm:"not null;uniqueIndex:idx_ns_org_name"`
	Name  string `gorm:"not null;uniqueIndex:idx_ns_org_name"`
}

// FindOrCreateOrg returns the org with this slug, creating it if needed.
func (s *Store) FindOrCreateOrg(name string) (*Organization, error) {
	var c Organization
	result := s.db.Where("name = ?", name).FirstOrCreate(&c, Organization{Name: name})
	return &c, result.Error
}

// GetOrgByID returns an org by primary key.
func (s *Store) GetOrgByID(id OrgID) (*Organization, error) {
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

// ValidateNamespaceName reports whether name can be a single namespace.
// A namespace is one path segment (/log/<ns>/...), a token scope, and the
// stem of the "<ns>+tokens" permission group, so a name that holds a list
// separator (comma), whitespace, a path slash, the "+" facet marker, a glob
// character, a control character, or that starts like a flag could never
// round-trip through those and is refused up front.
func ValidateNamespaceName(name string) error {
	if name == "" {
		return errors.New("namespace name is empty")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid namespace name %q: must not start with '-'", name)
	}
	for _, r := range name {
		switch {
		case r == ',' || r == '/' || r == '+' || r == '*' || r == '?' || r == '[':
			return fmt.Errorf("invalid namespace name %q: must not contain %q", name, string(r))
		case unicode.IsSpace(r) || unicode.IsControl(r):
			return fmt.Errorf("invalid namespace name %q: must not contain whitespace or control characters", name)
		}
	}
	return nil
}

// FindOrCreateNamespace returns the namespace, creating if needed.
func (s *Store) FindOrCreateNamespace(orgID OrgID, name string) (*Namespace, error) {
	var ns Namespace
	result := s.scoped(orgID).Where("name = ?", name).FirstOrCreate(&ns, Namespace{OrgID: orgID, Name: name})
	return &ns, result.Error
}

// GetNamespace looks up a namespace by (org_id, name) without creating
// one if absent. Callers validating user-supplied namespace names (e.g.
// AddSource) should use this so a typo doesn't silently spawn a new
// namespace under the org.
func (s *Store) GetNamespace(orgID OrgID, name string) (*Namespace, error) {
	var ns Namespace
	if err := s.scoped(orgID).Where("name = ?", name).First(&ns).Error; err != nil {
		return nil, err
	}
	return &ns, nil
}

// NamespaceContents is what stands between a namespace and its removal.
type NamespaceContents struct {
	Sources []string // service names, sorted
	Tokens  int      // live (unrevoked) agent tokens
}

// NamespaceContents lists a namespace's sources and counts its live tokens.
func (s *Store) NamespaceContents(orgID OrgID, namespaceID uint) (NamespaceContents, error) {
	var c NamespaceContents
	if err := s.scoped(orgID).Model(&LogGroup{}).Where("namespace_id = ?", namespaceID).
		Order("app").Pluck("app", &c.Sources).Error; err != nil {
		return c, err
	}
	var n int64
	if err := s.scoped(orgID).Model(&AgentToken{}).
		Where("namespace_id = ? AND revoked = false", namespaceID).Count(&n).Error; err != nil {
		return c, err
	}
	c.Tokens = int(n)
	return c, nil
}

// DeleteNamespace removes a namespace and everything keyed to it, for good.
// It is a hard delete on purpose: a soft delete would keep the (org, name)
// unique row, so the name could never be created again, and would not fire
// the foreign-key cascades that clear tokens, sources and shard routing.
// Permissions have no foreign key, so they are deleted explicitly.
func (s *Store) DeleteNamespace(ctx context.Context, orgID OrgID, name string) error {
	ns, err := s.GetNamespace(orgID, name)
	if err != nil {
		return err
	}
	var services []string
	if err := s.scoped(orgID).Model(&LogGroup{}).Where("namespace_id = ?", ns.ID).Pluck("app", &services).Error; err != nil {
		return err
	}
	err = s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
		for _, m := range []any{&NamespacePermission{}, &LogGroup{}, &AgentToken{}} {
			if err := tx.Unscoped().Where("namespace_id = ?", ns.ID).Delete(m).Error; err != nil {
				return err
			}
		}
		return tx.Unscoped().Delete(ns).Error
	})
	if err != nil {
		return err
	}
	for _, svc := range services {
		s.invalidateSchemaCache(ctx, ns.ID, svc)
	}
	return nil
}

// ListNamespaces returns all namespaces for an org.
func (s *Store) ListNamespaces(orgID OrgID) ([]Namespace, error) {
	var nss []Namespace
	err := s.scoped(orgID).Order("name").Find(&nss).Error
	return nss, err
}
