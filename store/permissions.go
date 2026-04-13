package store

import "gorm.io/gorm"

// NamespacePermission grants a user specific permissions on a namespace.
// Owners bypass this entirely — they have implicit full access.
// Members with no row for a namespace have no access to it.
type NamespacePermission struct {
	gorm.Model
	UserID                string `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	NamespaceID           uint   `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	CanRead               bool   `gorm:"not null;default:true"`
	CanCreateTokens       bool   `gorm:"not null;default:false"`
	CanManageWatches      bool   `gorm:"not null;default:false"`
	CanManageFunctions    bool   `gorm:"not null;default:false"`
	CanManageIntegrations bool   `gorm:"not null;default:false"`
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
	result := s.db.Where("user_id = ? AND namespace_id = ?", p.UserID, p.NamespaceID).First(&existing)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(p).Error
	}
	return s.db.Model(&existing).Updates(map[string]any{
		"can_read":                  p.CanRead,
		"can_create_tokens":         p.CanCreateTokens,
		"can_manage_watches":        p.CanManageWatches,
		"can_manage_functions":      p.CanManageFunctions,
		"can_manage_integrations":   p.CanManageIntegrations,
	}).Error
}

// DeleteNamespacePermission removes a permission record.
func (s *Store) DeleteNamespacePermission(userID string, namespaceID uint) error {
	return s.db.Where("user_id = ? AND namespace_id = ?", userID, namespaceID).Delete(&NamespacePermission{}).Error
}

// ReadableNamespaceIDs returns the namespace IDs a user can read.
// Returns nil for owners (meaning all namespaces).
func (s *Store) ReadableNamespaceIDs(userID string) ([]uint, error) {
	var ids []uint
	err := s.db.Model(&NamespacePermission{}).
		Where("user_id = ? AND can_read = ?", userID, true).
		Pluck("namespace_id", &ids).Error
	return ids, err
}

// GrantFullNamespaceAccess gives a user all permissions on a namespace.
func (s *Store) GrantFullNamespaceAccess(userID string, namespaceID uint) error {
	return s.UpsertNamespacePermission(&NamespacePermission{
		UserID:                userID,
		NamespaceID:           namespaceID,
		CanRead:               true,
		CanCreateTokens:       true,
		CanManageWatches:      true,
		CanManageFunctions:    true,
		CanManageIntegrations: true,
	})
}
