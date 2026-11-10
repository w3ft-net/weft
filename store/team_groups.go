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
//   - "<ns>-r" — read access to <ns>: its logs and its crons,
//     backed by NamespacePermission.CanRead.
//   - "<ns>-w" — manage <ns>: its crons, tokens and sources,
//     backed by NamespacePermission.CanWrite.
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

// The two groups of a namespace.
const (
	readSuffix  = "-r"
	writeSuffix = "-w"
)

// SplitNamespaceGroup parses "<ns>-r" and "<ns>-w". A name that ends either
// way always parses as that namespace's group, so "a-w" is a's write group and
// a namespace called "a-w" has "a-w-r" and "a-w-w".
func SplitNamespaceGroup(group string) (ns string, write, ok bool) {
	if ns, ok = strings.CutSuffix(group, readSuffix); ok && ns != "" {
		return ns, false, true
	}
	if ns, ok = strings.CutSuffix(group, writeSuffix); ok && ns != "" {
		return ns, true, true
	}
	return "", false, false
}

// AddUserToGroup grants (email, group) membership. group is a manager
// group, "<ns>-r" or "<ns>-w". Owner-only; the api layer gates
// the caller before reaching here.
func (s *Store) AddUserToGroup(orgID OrgID, email, group string) error {
	return s.setGroupMembership(orgID, email, group, true)
}

// RemoveUserFromGroup revokes (email, group) membership.
func (s *Store) RemoveUserFromGroup(orgID OrgID, email, group string) error {
	return s.setGroupMembership(orgID, email, group, false)
}

func (s *Store) setGroupMembership(orgID OrgID, email, group string, grant bool) error {
	uid, err := s.resolveGrantableMember(orgID, email)
	if err != nil {
		return err
	}

	if col, ok := managerGroupColumn[group]; ok {
		return s.setOrgBit(orgID, uid, col, grant)
	}

	nsName, write, ok := SplitNamespaceGroup(group)
	if !ok {
		return fmt.Errorf("no such group or namespace: %s (a namespace's groups are %s-r and %s-w)", group, group, group)
	}
	ns, err := s.findNamespace(orgID, nsName)
	if err != nil {
		return err
	}
	return s.setNamespaceBit(uid, ns.ID, write, grant)
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
func (s *Store) resolveGrantableMember(orgID OrgID, email string) (string, error) {
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
// existing namespace's read or write group. It touches no
// member, so callers can reject a bad group before any side effect —
// e.g. before inviting someone on what turns out to be a typo.
func (s *Store) ValidateGroup(orgID OrgID, group string) error {
	if _, ok := managerGroupColumn[group]; ok {
		return nil
	}
	nsName, _, ok := SplitNamespaceGroup(group)
	if !ok {
		return fmt.Errorf("no such group or namespace: %s (a namespace's groups are %s-r and %s-w)", group, group, group)
	}
	_, err := s.findNamespace(orgID, nsName)
	return err
}

// findNamespace resolves a namespace by name within a customer without
// creating it — an unknown name is a user error, not a new namespace.
func (s *Store) findNamespace(orgID OrgID, name string) (*Namespace, error) {
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
func (s *Store) setOrgBit(orgID OrgID, userID, column string, val bool) error {
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

// setNamespaceBit sets one facet (read or write) of a user's
// namespace permission, preserving the other. When both facets end up
// false the row is deleted so an empty grant leaves no trace.
func (s *Store) setNamespaceBit(userID string, nsID uint, write bool, val bool) error {
	read, create := false, false
	existing, err := s.GetNamespacePermission(userID, nsID)
	if err == nil {
		read, create = existing.CanRead, existing.CanWrite
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if write {
		create = val
	} else {
		read = val
	}
	if !read && !create {
		return s.db.Where("user_id = ? AND namespace_id = ?", userID, nsID).
			Delete(&NamespacePermission{}).Error
	}
	return s.UpsertNamespacePermission(&NamespacePermission{
		UserID:      userID,
		NamespaceID: nsID,
		CanRead:     read,
		CanWrite:    create,
	})
}

// IsOwner reports whether userID is an owner of orgID. A
// non-member (no row) is not an owner, not an error.
func (s *Store) IsOwner(orgID OrgID, userID string) (bool, error) {
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
// ErrOwnerImmutable: an owner can't be removed from their org.
var ErrOwnerImmutable = errors.New("owner cannot be removed")

func (s *Store) RemoveMember(orgID OrgID, email string) error {
	m, err := s.GetTeamMemberByEmail(orgID, email)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("no such member: %s", email)
	}
	if err != nil {
		return err
	}
	if m.Role == RoleOwner {
		return fmt.Errorf("%s is an owner and cannot be removed: %w", email, ErrOwnerImmutable)
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
		// namespace_permissions carries no org, only a namespace id: limit the
		// delete to this org's namespaces or it wipes the user's grants in
		// every other org they belong to.
		orgNamespaces := txStore.scoped(orgID).Model(&Namespace{}).Select("id")
		return tx.Where("user_id = ? AND namespace_id IN (?)", m.UserID, orgNamespaces).
			Delete(&NamespacePermission{}).Error
	})
}

// memberGroups returns the sorted group tokens a member belongs to:
// manager groups first (binmgr/cronmgr/configmgr/nsmgr in that order),
// then namespace groups alphabetically with its write group following its
// base. Pending invitees return nil — they hold no grants yet.
//
// Owners go through the same path as everyone else. Their groups are
// real rows now, so there is no owner fork here and none at the
// rendering sites either.
func (s *Store) memberGroups(orgID OrgID, m TeamMember, nsByID map[uint]string) ([]string, error) {
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
		name  string
		write bool
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
		if np.CanWrite {
			nsGroups = append(nsGroups, nsGroup{name, true})
		}
	}
	sort.Slice(nsGroups, func(i, j int) bool {
		if nsGroups[i].name != nsGroups[j].name {
			return nsGroups[i].name < nsGroups[j].name
		}
		return !nsGroups[i].write // read before write
	})
	for _, g := range nsGroups {
		if g.write {
			groups = append(groups, g.name+writeSuffix)
		} else {
			groups = append(groups, g.name+readSuffix)
		}
	}
	return groups, nil
}

// MemberGroups returns the group tokens for a single member by email,
// along with their role and status — the backing query for `groups`
// and `id`.
func (s *Store) MemberGroups(orgID OrgID, email string) (role, status string, groups []string, err error) {
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
func (s *Store) RenderPasswd(orgID OrgID) (string, error) {
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

	// Each namespace's cron user, the identity its crons fire as.
	for _, ns := range nsByID {
		user := ns + cronUserSuffix
		groups, err := s.CronUserGroups(orgID, user)
		if err != nil {
			return "", err
		}
		groupsCol := "—"
		if len(groups) > 0 {
			groupsCol = strings.Join(groups, "  ")
		}
		rows = append(rows, row{user, "cron", "system", groupsCol})
		if len(user) > userW {
			userW = len(user)
		}
	}
	sort.SliceStable(rows[len(members):], func(i, j int) bool { return rows[len(members)+i].user < rows[len(members)+j].user })

	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-7s  %-8s  %s\n", userW, "USER", "ROLE", "STATUS", "GROUPS")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %-7s  %-8s  %s\n", userW, r.user, r.role, r.status, r.groups)
	}
	return b.String(), nil
}

// namespacesByID returns a customer's namespaces keyed by ID, for
// resolving permission rows back to namespace names.
func (s *Store) namespacesByID(orgID OrgID) (map[uint]string, error) {
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
