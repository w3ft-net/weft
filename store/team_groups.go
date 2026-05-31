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
//   - a manager group (binmgr / cronmgr / configmgr) — edit access to
//     /bin, /cron.d, /etc, backed by the OrgPermission bits.
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
}

// tokensSuffix marks the produce/token facet of a namespace group. It
// uses "+" rather than ":" so the token survives the colon-delimited
// /etc renders and can never collide with a real namespace name ("+"
// is illegal in namespaces).
const tokensSuffix = "+tokens"

// AddUserToGroup grants (email, group) membership. group is a manager
// group, a namespace, or "<ns>+tokens". Owner-only; the api layer gates
// the caller before reaching here.
func (s *Store) AddUserToGroup(customerID uint, email, group string) error {
	return s.setGroupMembership(customerID, email, group, true)
}

// RemoveUserFromGroup revokes (email, group) membership.
func (s *Store) RemoveUserFromGroup(customerID uint, email, group string) error {
	return s.setGroupMembership(customerID, email, group, false)
}

func (s *Store) setGroupMembership(customerID uint, email, group string, grant bool) error {
	uid, err := s.resolveGrantableMember(customerID, email)
	if err != nil {
		return err
	}

	if col, ok := managerGroupColumn[group]; ok {
		return s.setOrgBit(customerID, uid, col, grant)
	}

	tokens := strings.HasSuffix(group, tokensSuffix)
	nsName := strings.TrimSuffix(group, tokensSuffix)
	ns, err := s.findNamespace(customerID, nsName)
	if err != nil {
		return err
	}
	return s.setNamespaceBit(uid, ns.ID, tokens, grant)
}

// resolveGrantableMember validates that email names an active member
// who can hold permissions, returning their user ID. Owners and
// not-yet-accepted invitees are rejected with actionable errors.
func (s *Store) resolveGrantableMember(customerID uint, email string) (string, error) {
	m, err := s.GetTeamMemberByEmail(customerID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("no such member: %s (invite them first: adduser %s)", email, email)
	}
	if err != nil {
		return "", err
	}
	if m.Role == "owner" {
		return "", fmt.Errorf("%s is an owner — owners have implicit full access", email)
	}
	if m.UserID == nil {
		return "", fmt.Errorf("%s has been invited but hasn't accepted yet — grant groups after they accept", email)
	}
	return *m.UserID, nil
}

// ValidateGroup reports whether group names a real manager group or an
// existing namespace (with optional +tokens facet). It touches no
// member, so callers can reject a bad group before any side effect —
// e.g. before inviting someone on what turns out to be a typo.
func (s *Store) ValidateGroup(customerID uint, group string) error {
	if _, ok := managerGroupColumn[group]; ok {
		return nil
	}
	nsName := strings.TrimSuffix(group, tokensSuffix)
	_, err := s.findNamespace(customerID, nsName)
	return err
}

// findNamespace resolves a namespace by name within a customer without
// creating it — an unknown name is a user error, not a new namespace.
func (s *Store) findNamespace(customerID uint, name string) (*Namespace, error) {
	namespaces, err := s.ListNamespaces(customerID)
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
// the other two. Creates the row if absent.
func (s *Store) setOrgBit(customerID uint, userID, column string, val bool) error {
	var existing OrgPermission
	err := s.scoped(customerID).Where("user_id = ?", userID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.scoped(customerID).Model(&OrgPermission{}).Create(map[string]any{
			"customer_id": customerID,
			"user_id":     userID,
			column:        val,
		}).Error
	}
	if err != nil {
		return err
	}
	return s.scoped(customerID).Model(&existing).Update(column, val).Error
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

// IsOwner reports whether userID is an owner of customerID. A
// non-member (no row) is not an owner, not an error.
func (s *Store) IsOwner(customerID uint, userID string) (bool, error) {
	m, err := s.GetTeamMemberByUserID(customerID, userID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.Role == "owner", nil
}

// RemoveMember removes a member from the org entirely: their membership
// is marked removed and their org + namespace permissions are dropped.
// Owners cannot be removed this way. Returns an error if email names no
// member or an owner.
func (s *Store) RemoveMember(customerID uint, email string) error {
	m, err := s.GetTeamMemberByEmail(customerID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("no such member: %s", email)
	}
	if err != nil {
		return err
	}
	if m.Role == "owner" {
		return fmt.Errorf("%s is an owner and cannot be removed", email)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		txStore := &Store{db: tx}
		if err := txStore.scoped(customerID).Model(&TeamMember{}).Where("id = ?", m.ID).
			Update("status", "removed").Error; err != nil {
			return err
		}
		if m.UserID == nil {
			return nil
		}
		uid := *m.UserID
		if err := txStore.scoped(customerID).Where("user_id = ?", uid).
			Delete(&OrgPermission{}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ?", uid).Delete(&NamespacePermission{}).Error
	})
}

// memberGroups returns the sorted group tokens a member belongs to:
// manager groups first (binmgr/cronmgr/configmgr in that order), then
// namespace groups alphabetically with "+tokens" following its base.
// Owners and pending invitees return nil — they hold no explicit
// groups.
func (s *Store) memberGroups(customerID uint, m TeamMember, nsByID map[uint]string) ([]string, error) {
	if m.Role == "owner" || m.UserID == nil {
		return nil, nil
	}
	uid := *m.UserID

	var groups []string
	op, err := s.GetOrgPermission(customerID, uid)
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
func (s *Store) MemberGroups(customerID uint, email string) (role, status string, groups []string, err error) {
	m, err := s.GetTeamMemberByEmail(customerID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", nil, fmt.Errorf("no such member: %s", email)
	}
	if err != nil {
		return "", "", nil, err
	}
	nsByID, err := s.namespacesByID(customerID)
	if err != nil {
		return "", "", nil, err
	}
	groups, err = s.memberGroups(customerID, *m, nsByID)
	return m.Role, m.Status, groups, err
}

// RenderPasswd renders the read-only /etc/passwd roster table: one row
// per member with role, status, and group membership. Owners show
// "(all — implicit)"; members with no groups show "—".
func (s *Store) RenderPasswd(customerID uint) (string, error) {
	members, err := s.ListTeamMembers(customerID)
	if err != nil {
		return "", err
	}
	nsByID, err := s.namespacesByID(customerID)
	if err != nil {
		return "", err
	}

	type row struct{ user, role, status, groups string }
	rows := make([]row, 0, len(members))
	userW := len("USER")
	for _, m := range members {
		groupsCol := "—"
		switch {
		case m.Role == "owner":
			groupsCol = "(all — implicit)"
		default:
			g, err := s.memberGroups(customerID, m, nsByID)
			if err != nil {
				return "", err
			}
			if len(g) > 0 {
				groupsCol = strings.Join(g, "  ")
			}
		}
		status := m.Status
		if status == "pending" {
			status = "invited"
		}
		rows = append(rows, row{m.Email, m.Role, status, groupsCol})
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
func (s *Store) namespacesByID(customerID uint) (map[uint]string, error) {
	namespaces, err := s.ListNamespaces(customerID)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]string, len(namespaces))
	for _, n := range namespaces {
		byID[n.ID] = n.Name
	}
	return byID, nil
}
