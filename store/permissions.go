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
	UserID      string `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	NamespaceID uint   `gorm:"not null;uniqueIndex:idx_nsperm_user_ns"`
	CanRead     bool   `gorm:"not null;default:true"`
	CanWrite    bool   `gorm:"not null;default:false"`
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

// NamespaceNames returns the names of the org's namespaces, ordered by name.
// With all set (owners) every namespace is returned; otherwise only those the
// user has read permission on.
func (s *Store) NamespaceNames(orgID OrgID, userID string, all bool) ([]string, error) {
	var names []string
	err := s.scoped(orgID).Model(&Namespace{}).
		Joins("LEFT JOIN namespace_permissions p ON p.namespace_id = namespaces.id AND p.user_id = ? AND p.deleted_at IS NULL", userID).
		Where("? OR p.can_read", all).
		Order("namespaces.name").
		Pluck("namespaces.name", &names).Error
	return names, err
}

// ReadableNamespaces is the one rule for which namespaces a user may read: all
// of them for an owner and for the empty user (the system identity crons and
// internal callers run as), otherwise those they hold read permission on.
// Everything that decides read access — the /log listing, log queries — asks
// this. The caller says whether the user is an owner, since it already knows
// (the session, or Store.IsOwner).
func (s *Store) ReadableNamespaces(orgID OrgID, userID string, owner bool) ([]string, error) {
	return s.NamespaceNames(orgID, userID, owner || userID == "")
}

// UpsertNamespacePermission creates or updates a namespace permission.
func (s *Store) UpsertNamespacePermission(p *NamespacePermission) error {
	var existing NamespacePermission
	vals := map[string]any{
		"user_id":      p.UserID,
		"namespace_id": p.NamespaceID,
		"can_read":     p.CanRead,
		"can_write":    p.CanWrite,
	}
	// Unscoped: a revoked grant is a soft-deleted row that still holds the
	// (user, namespace) key, so granting again revives it.
	result := s.db.Unscoped().Where("user_id = ? AND namespace_id = ?", p.UserID, p.NamespaceID).First(&existing)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Model(&NamespacePermission{}).Create(vals).Error
	}
	if result.Error != nil {
		return result.Error
	}
	vals["deleted_at"] = nil
	return s.db.Unscoped().Model(&existing).Updates(vals).Error
}

// GrantFullNamespaceAccess gives a user the maximum namespace-scoped
// access. Used by signup flows to grant a fresh user's own namespaces
// to themselves before they switch on the org role.
func (s *Store) GrantFullNamespaceAccess(userID string, namespaceID uint) error {
	return s.UpsertNamespacePermission(&NamespacePermission{
		UserID:      userID,
		NamespaceID: namespaceID,
		CanRead:     true,
		CanWrite:    true,
	})
}

// OrgPermission grants a user the org-scoped privileges: write access to
// /bin and /etc (the binmgr / configmgr groups) and the
// right to create namespaces (nsmgr). Members with no row default to
// read-only across all four. Granting these (via adduser/deluser or the
// dashboard Members grid) is owner-only.
type OrgPermission struct {
	gorm.Model
	OrgID               OrgID  `gorm:"not null;uniqueIndex:idx_orgperm_org_user"`
	UserID              string `gorm:"not null;uniqueIndex:idx_orgperm_org_user"`
	CanManageFunctions  bool   `gorm:"not null;default:false"`
	CanManageEtc        bool   `gorm:"not null;default:false"`
	CanCreateNamespaces bool   `gorm:"not null;default:false"`
}

// GetOrgPermission returns the org-scope permissions for a user in a
// customer. Returns gorm.ErrRecordNotFound when no row exists; callers
// translate "not found" into the default-deny (all-false) state.
func (s *Store) GetOrgPermission(orgID OrgID, userID string) (*OrgPermission, error) {
	var p OrgPermission
	err := s.scoped(orgID).Where("user_id = ?", userID).First(&p).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}
