package store

import "gorm.io/gorm"

// NamespacePermission grants a user permissions on a single namespace.
// Owners bypass this entirely — they have implicit full access.
// Members with no row for a namespace have no access to it.
//
// Org-scoped concepts (managing /bin scripts, /cron.d watches, /etc
// files) live on OrgPermission, not here — those directories are
// org-wide, not per-namespace.
type NamespacePermission struct {
	gorm.Model
	UserID          string `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	NamespaceID     uint   `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	CanRead         bool   `gorm:"not null;default:true"`
	CanCreateTokens bool   `gorm:"not null;default:false"`
}

// GetNamespacePermission returns the permission record for a user + namespace.
func (s *Store) GetNamespacePermission(userID string, namespaceID uint) (*NamespacePermission, error) {
	var p NamespacePermission
	err := s.db.Where("user_id = ? AND namespace_id = ?", userID, namespaceID).First(&p).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListUserNamespacePermissions returns all namespace permissions for a user.
func (s *Store) ListUserNamespacePermissions(userID string) ([]NamespacePermission, error) {
	var perms []NamespacePermission
	err := s.db.Where("user_id = ?", userID).Find(&perms).Error
	return perms, err
}

// UpsertNamespacePermission creates or updates a namespace permission.
func (s *Store) UpsertNamespacePermission(p *NamespacePermission) error {
	var existing NamespacePermission
	vals := map[string]any{
		"user_id":           p.UserID,
		"namespace_id":      p.NamespaceID,
		"can_read":          p.CanRead,
		"can_create_tokens": p.CanCreateTokens,
	}
	result := s.db.Where("user_id = ? AND namespace_id = ?", p.UserID, p.NamespaceID).First(&existing)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Model(&NamespacePermission{}).Create(vals).Error
	}
	return s.db.Model(&existing).Updates(vals).Error
}

// GrantFullNamespaceAccess gives a user the maximum namespace-scoped
// access. Used by signup flows to grant a fresh user's own namespaces
// to themselves before they switch on the org role.
func (s *Store) GrantFullNamespaceAccess(userID string, namespaceID uint) error {
	return s.UpsertNamespacePermission(&NamespacePermission{
		UserID:          userID,
		NamespaceID:     namespaceID,
		CanRead:         true,
		CanCreateTokens: true,
	})
}

// OrgPermission grants a user write access to one or more of the
// org-scoped directories (/bin, /cron.d, /etc). Owners bypass this
// entirely. Members with no row default to read-only across all
// three. /etc/team.yaml writes require the owner role regardless of
// CanManageEtc — only owners can grant or revoke perms.
type OrgPermission struct {
	gorm.Model
	CustomerID         uint   `gorm:"not null;uniqueIndex:idx_orgperm_customer_user"`
	UserID             string `gorm:"not null;uniqueIndex:idx_orgperm_customer_user"`
	CanManageFunctions bool   `gorm:"not null;default:false"`
	CanManageWatches   bool   `gorm:"not null;default:false"`
	CanManageEtc       bool   `gorm:"not null;default:false"`
}

// GetOrgPermission returns the org-scope permissions for a user in a
// customer. Returns gorm.ErrRecordNotFound when no row exists; callers
// translate "not found" into the default-deny (all-false) state.
func (s *Store) GetOrgPermission(customerID uint, userID string) (*OrgPermission, error) {
	var p OrgPermission
	err := s.scoped(customerID).Where("user_id = ?", userID).First(&p).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}
