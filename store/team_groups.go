package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// Team management is modeled as Unix-style group membership. A group is
// one of three things:
//
//   - a manager group (binmgr / cronmgr / configmgr / nsmgr) — edit access
//     to /bin, /cron.d, /etc and the right to create namespaces, backed by
//     the OrgPermission bits.
//   - a namespace name — read access to /log/<ns>, backed by
//     NamespacePermission.CanRead.
//   - "<ns>+tokens" — permission to mint producer tokens for <ns>,
//     backed by NamespacePermission.CanCreateTokens.
//
// adduser/deluser mutate one membership at a time; /etc/passwd renders
// the whole roster read-only. Owners are never represented in groups —
// they have implicit full access.

// managerGroupColumn maps a manager-group name to the OrgPermission
// column it toggles.
var managerGroupColumn = map[string]string{
	"binmgr":    "can_manage_functions",
	"cronmgr":   "can_manage_watches",
	"configmgr": "can_manage_etc",
	"nsmgr":     "can_create_namespaces",
}

// tokensSuffix marks the produce/token facet of a namespace group. It
// uses "+" rather than ":" so the token survives the colon-delimited
// /etc renders and can never collide with a real namespace name ("+"
// is illegal in namespaces).
const tokensSuffix = "+tokens"

// AddUserToGroup grants (email, group) membership. group is a manager
// group, a namespace, or "<ns>+tokens". Owner-only; the api layer gates
// the caller before reaching here.
func (s *Store) AddUserToGroup(orgID string, email, group string) error {
	return s.setGroupMembership(orgID, email, group, true)
}

// RemoveUserFromGroup revokes (email, group) membership.
func (s *Store) RemoveUserFromGroup(orgID string, email, group string) error {
	return s.setGroupMembership(orgID, email, group, false)
}

func (s *Store) setGroupMembership(orgID string, email, group string, grant bool) error {
	uid, err := s.resolveGrantableMember(orgID, email)
	if err != nil {
		return err
	}

	if col, ok := managerGroupColumn[group]; ok {
		return s.setOrgBit(orgID, uid, col, grant)
	}

	tokens := strings.HasSuffix(group, tokensSuffix)
	nsName := strings.TrimSuffix(group, tokensSuffix)
	ns, err := s.findNamespace(orgID, nsName)
	if err != nil {
		return err
	}
	return s.setNamespaceBit(uid, ns.ID, tokens, grant)
}

// resolveGrantableMember validates that email names an active member
// who can hold permissions, returning their user ID. Not-yet-accepted
// invitees are rejected with an actionable error.
//
// Owners are grantable. They used to be refused — there was nothing to
// grant to, since their access was derived from the role — which also
// meant `deluser <owner> binmgr` had no way to do anything. Since
// migration 072 an owner holds the same four bits as anyone else, so
// both directions work.
func (s *Store) resolveGrantableMember(orgID string, email string) (string, error) {
	m, err := s.GetTeamMemberByEmail(orgID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("no such member: %s (invite them first: adduser %s)", email, email)
	}
	if err != nil {
		return "", err
	}
	if m.Pending() {
		return "", fmt.Errorf("%s has been invited but hasn't accepted yet — grant groups after they accept", email)
	}
	return m.UserID, nil
}

// ValidateGroup reports whether group names a real manager group or an
// existing namespace (with optional +tokens facet). It touches no
// member, so callers can reject a bad group before any side effect —
// e.g. before inviting someone on what turns out to be a typo.
func (s *Store) ValidateGroup(orgID string, group string) error {
	if _, ok := managerGroupColumn[group]; ok {
		return nil
	}
	nsName := strings.TrimSuffix(group, tokensSuffix)
	_, err := s.findNamespace(orgID, nsName)
	return err
}

// findNamespace resolves a namespace by name within a customer without
// creating it — an unknown name is a user error, not a new namespace.
func (s *Store) findNamespace(orgID string, name string) (*Namespace, error) {
	namespaces, err := s.ListNamespaces(orgID)
	if err != nil {
		return nil, err
	}
	for i := range namespaces {
		if namespaces[i].Name == name {
			return &namespaces[i], nil
		}
	}
	return nil, fmt.Errorf("no such group or namespace: %s", name)
}

// setOrgBit sets a single OrgPermission column for a user, preserving
// the others. Creates the row if absent, and revives it if tombstoned.
//
// The tombstone case is the whole reason this isn't a plain upsert. A
// removed member's row is soft-deleted, but idx_orgperm_org_user has no
// deleted_at predicate, so the row still occupies (org_id, user_id): a
// soft-delete-scoped lookup misses it, and the Create that follows
// collides. Migration 072 moved removal into a trigger, which makes
// tombstones the normal state for anyone who has ever been removed.
//
// Reviving clears the other bits rather than restoring them. Re-adding a
// member restores the membership, not the grants — the tombstone is a
// record of what they used to hold, not an entitlement.
func (s *Store) setOrgBit(orgID string, userID, column string, val bool) error {
	var existing OrgPermission
	err := s.scoped(orgID).Unscoped().Where("user_id = ?", userID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.scoped(orgID).Model(&OrgPermission{}).Create(map[string]any{
			"org_id":  orgID,
			"user_id": userID,
			column:    val,
		}).Error
	}
	if err != nil {
		return err
	}
	if existing.DeletedAt.Valid {
		return s.scoped(orgID).Unscoped().Model(&existing).Updates(map[string]any{
			"can_manage_functions":  false,
			"can_manage_watches":    false,
			"can_manage_etc":        false,
			"can_create_namespaces": false,
			column:                  val,
			"deleted_at":            nil,
		}).Error
	}
	return s.scoped(orgID).Model(&existing).Update(column, val).Error
}

// setNamespaceBit sets one facet (read or create-tokens) of a user's
// namespace permission, preserving the other. When both facets end up
// false the row is deleted so an empty grant leaves no trace.
func (s *Store) setNamespaceBit(userID string, nsID uint, tokens bool, val bool) error {
	read, create := false, false
	existing, err := s.GetNamespacePermission(userID, nsID)
	if err == nil {
		read, create = existing.CanRead, existing.CanCreateTokens
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if tokens {
		create = val
	} else {
		read = val
	}
	if !read && !create {
		return s.db.Where("user_id = ? AND namespace_id = ?", userID, nsID).
			Delete(&NamespacePermission{}).Error
	}
	return s.UpsertNamespacePermission(&NamespacePermission{
		UserID:          userID,
		NamespaceID:     nsID,
		CanRead:         read,
		CanCreateTokens: create,
	})
}

// IsOwner reports whether userID is an owner of orgID. A
// non-member (no row) is not an owner, not an error.
func (s *Store) IsOwner(orgID string, userID string) (bool, error) {
	m, err := s.GetTeamMemberByUserID(orgID, userID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.Role == RoleOwner, nil
}

// RemoveMember removes a member from the org entirely: their membership
// is marked removed and their namespace permissions are dropped. Owners
// cannot be removed this way. Returns an error if email names no member
// or an owner.
//
// The org_permissions tombstone is not written here — migration 072's
// trigger does it, on the same status update, inside the same
// transaction. Two writers for one row is how the tombstone bug in
// setOrgBit stayed hidden; the trigger is now the only one.
func (s *Store) RemoveMember(orgID string, email string) error {
	m, err := s.GetTeamMemberByEmail(orgID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("no such member: %s", email)
	}
	if err != nil {
		return err
	}
	if m.Role == RoleOwner {
		return fmt.Errorf("%s is an owner and cannot be removed", email)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		txStore := &Store{db: tx}
		if err := txStore.scoped(orgID).Model(&TeamMember{}).Where("id = ?", m.ID).
			Update("status", "removed").Error; err != nil {
			return err
		}
		if m.Pending() {
			return nil
		}
		return tx.Where("user_id = ?", m.UserID).Delete(&NamespacePermission{}).Error
	})
}

// memberGroups returns the sorted group tokens a member belongs to:
// manager groups first (binmgr/cronmgr/configmgr/nsmgr in that order),
// then namespace groups alphabetically with "+tokens" following its
// base. Pending invitees return nil — they hold no grants yet.
//
// Owners go through the same path as everyone else. Their groups are
// real rows now, so there is no owner fork here and none at the
// rendering sites either.
func (s *Store) memberGroups(orgID string, m TeamMember, nsByID map[uint]string) ([]string, error) {
	if m.Pending() {
		return nil, nil
	}
	uid := m.UserID

	var groups []string
	op, err := s.GetOrgPermission(orgID, uid)
	if err == nil && op != nil {
		if op.CanManageFunctions {
			groups = append(groups, "binmgr")
		}
		if op.CanManageWatches {
			groups = append(groups, "cronmgr")
		}
		if op.CanManageEtc {
			groups = append(groups, "configmgr")
		}
		if op.CanCreateNamespaces {
			groups = append(groups, "nsmgr")
		}
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	nsPerms, err := s.ListUserNamespacePermissions(uid)
	if err != nil {
		return nil, err
	}
	type nsGroup struct {
		name   string
		tokens bool
	}
	var nsGroups []nsGroup
	for _, np := range nsPerms {
		name, ok := nsByID[np.NamespaceID]
		if !ok {
			continue // stale row pointing at a deleted namespace
		}
		if np.CanRead {
			nsGroups = append(nsGroups, nsGroup{name, false})
		}
		if np.CanCreateTokens {
			nsGroups = append(nsGroups, nsGroup{name, true})
		}
	}
	sort.Slice(nsGroups, func(i, j int) bool {
		if nsGroups[i].name != nsGroups[j].name {
			return nsGroups[i].name < nsGroups[j].name
		}
		return !nsGroups[i].tokens // base before +tokens
	})
	for _, g := range nsGroups {
		if g.tokens {
			groups = append(groups, g.name+tokensSuffix)
		} else {
			groups = append(groups, g.name)
		}
	}
	return groups, nil
}

// MemberGroups returns the group tokens for a single member by email,
// along with their role and status — the backing query for `groups`
// and `id`.
func (s *Store) MemberGroups(orgID string, email string) (role, status string, groups []string, err error) {
	m, err := s.GetTeamMemberByEmail(orgID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", nil, fmt.Errorf("no such member: %s", email)
	}
	if err != nil {
		return "", "", nil, err
	}
	nsByID, err := s.namespacesByID(orgID)
	if err != nil {
		return "", "", nil, err
	}
	groups, err = s.memberGroups(orgID, *m, nsByID)
	return string(m.Role), string(m.Status), groups, err
}

// RenderPasswd renders the read-only /etc/passwd roster table: one row
// per member with role, status, and group membership. Members with no
// groups show "—", owners included — an owner's groups are rows like
// anyone else's since migration 072, so the "(all — implicit)" special
// case is gone.
func (s *Store) RenderPasswd(orgID string) (string, error) {
	members, err := s.ListTeamMembers(orgID)
	if err != nil {
		return "", err
	}
	nsByID, err := s.namespacesByID(orgID)
	if err != nil {
		return "", err
	}

	type row struct{ user, role, status, groups string }
	rows := make([]row, 0, len(members))
	userW := len("USER")
	for _, m := range members {
		groupsCol := "—"
		g, err := s.memberGroups(orgID, m, nsByID)
		if err != nil {
			return "", err
		}
		if len(g) > 0 {
			groupsCol = strings.Join(g, "  ")
		}
		status := string(m.Status)
		if m.Pending() {
			status = "invited"
		}
		rows = append(rows, row{m.Email, string(m.Role), status, groupsCol})
		if len(m.Email) > userW {
			userW = len(m.Email)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-7s  %-8s  %s\n", userW, "USER", "ROLE", "STATUS", "GROUPS")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %-7s  %-8s  %s\n", userW, r.user, r.role, r.status, r.groups)
	}
	return b.String(), nil
}

// namespacesByID returns a customer's namespaces keyed by ID, for
// resolving permission rows back to namespace names.
func (s *Store) namespacesByID(orgID string) (map[uint]string, error) {
	namespaces, err := s.ListNamespaces(orgID)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]string, len(namespaces))
	for _, n := range namespaces {
		byID[n.ID] = n.Name
	}
	return byID, nil
}
