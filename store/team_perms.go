package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// TeamPermsDoc is the JSON shape of the permissions API
// (GET/PUT /api/permissions) — one entry per non-owner team member,
// listing their org-scoped flags and per-namespace permissions. Owners
// are not represented: they have implicit full access, and including
// them would invite the reader into thinking they could revoke their
// own access.
//
// The map is keyed by email. The browser's Permissions grid is the
// only writer; the terminal mutates the same underlying rows one
// membership at a time via adduser/deluser (see team_groups.go).
type TeamPermsDoc map[string]MemberPerms

// MemberPerms is one member's record — per-directory access for
// /bin /cron.d /etc plus per-namespace permissions. Each *Access is
// "edit" or "read-only"; the namespaces map is omitted when empty.
type MemberPerms struct {
	Bin        AccessLevel             `json:"bin"`
	Cron       AccessLevel             `json:"cron"`
	Etc        AccessLevel             `json:"etc"`
	Namespaces map[string]NamespaceAcl `json:"namespaces,omitempty"`
}

// AccessLevel is a typed string with two valid values, "edit" and
// "read-only", mapping onto the CanManage<X> booleans.
type AccessLevel string

const (
	AccessEdit     AccessLevel = "edit"
	AccessReadOnly AccessLevel = "read-only"
)

// CanManage reports whether this level allows mutation.
func (a AccessLevel) CanManage() bool { return a == AccessEdit }

// NamespaceAcl is the per-namespace permission row. Both fields are
// boolean; missing == false.
type NamespaceAcl struct {
	Read         bool `json:"read"`
	CreateTokens bool `json:"create_tokens,omitempty"`
}

// BuildTeamPerms synthesizes the doc from store state — for every
// active team member of the customer, it joins their OrgPermission row
// (defaults to all read-only) and their NamespacePermission rows.
//
// Owners are included. They used to be omitted because their access was
// implicit and there was nothing to show; since migration 072 an admin
// holds real grants like everyone else, so leaving them out would hide
// rows that exist and make the doc a partial view of the truth it
// claims to be authoritative over.
func (s *Store) BuildTeamPerms(orgID string) (TeamPermsDoc, error) {
	members, err := s.ListTeamMembers(orgID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}

	namespaces, err := s.ListNamespaces(orgID)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	nsByID := make(map[uint]string, len(namespaces))
	for _, n := range namespaces {
		nsByID[n.ID] = n.Name
	}

	doc := TeamPermsDoc{}
	for _, m := range members {
		if m.Pending() {
			continue
		}
		uid := m.UserID
		mp := MemberPerms{
			Bin:  AccessReadOnly,
			Cron: AccessReadOnly,
			Etc:  AccessReadOnly,
		}
		if op, err := s.GetOrgPermission(orgID, uid); err == nil && op != nil {
			if op.CanManageFunctions {
				mp.Bin = AccessEdit
			}
			if op.CanManageWatches {
				mp.Cron = AccessEdit
			}
			if op.CanManageEtc {
				mp.Etc = AccessEdit
			}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("load org perms for %s: %w", m.Email, err)
		}

		nsPerms, err := s.ListUserNamespacePermissions(uid)
		if err != nil {
			return nil, fmt.Errorf("load ns perms for %s: %w", m.Email, err)
		}
		if len(nsPerms) > 0 {
			mp.Namespaces = make(map[string]NamespaceAcl, len(nsPerms))
			for _, np := range nsPerms {
				name, ok := nsByID[np.NamespaceID]
				if !ok {
					// Stale permission row pointing at a deleted
					// namespace — skip silently rather than emitting
					// an unresolvable namespace name.
					continue
				}
				mp.Namespaces[name] = NamespaceAcl{
					Read:         np.CanRead,
					CreateTokens: np.CanCreateTokens,
				}
			}
		}
		doc[m.Email] = mp
	}
	return doc, nil
}

// ApplyTeamPerms validates the doc against the customer's team
// membership + namespace list, then upserts both org and namespace
// permissions in a single transaction. Missing-from-doc is taken to
// mean "default-deny" — perms not in the doc are deleted from the
// store. Two-way authoritative: what's in the doc is what the user
// gets, full stop.
func (s *Store) ApplyTeamPerms(orgID string, doc TeamPermsDoc) error {
	members, err := s.ListTeamMembers(orgID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	memberByEmail := make(map[string]TeamMember, len(members))
	for _, m := range members {
		memberByEmail[m.Email] = m
	}

	namespaces, err := s.ListNamespaces(orgID)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	nsByName := make(map[string]Namespace, len(namespaces))
	for _, n := range namespaces {
		nsByName[n.Name] = n
	}

	// Validate up-front so a bad doc doesn't half-apply.
	for email, mp := range doc {
		m, ok := memberByEmail[email]
		if !ok {
			return fmt.Errorf("unknown team member: %s", email)
		}
		if m.Pending() {
			return fmt.Errorf("%s has not accepted the invite yet — permissions can be set after they sign up", email)
		}
		for nsName := range mp.Namespaces {
			if _, ok := nsByName[nsName]; !ok {
				return fmt.Errorf("%s: unknown namespace %q", email, nsName)
			}
		}
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		txStore := &Store{db: tx}
		// Apply per member: upsert OrgPermission, then reconcile
		// NamespacePermission rows (upsert listed, delete unlisted).
		for email, mp := range doc {
			m := memberByEmail[email]
			uid := m.UserID
			var existing OrgPermission
			vals := map[string]any{
				"org_id":               orgID,
				"user_id":              uid,
				"can_manage_functions": mp.Bin.CanManage(),
				"can_manage_watches":   mp.Cron.CanManage(),
				"can_manage_etc":       mp.Etc.CanManage(),
			}
			var err error
			if txStore.scoped(orgID).Where("user_id = ?", uid).First(&existing).Error == gorm.ErrRecordNotFound {
				err = txStore.scoped(orgID).Model(&OrgPermission{}).Create(vals).Error
			} else {
				err = txStore.scoped(orgID).Model(&existing).Updates(vals).Error
			}
			if err != nil {
				return err
			}
			if err := reconcileNsPerms(txStore, uid, mp.Namespaces, nsByName); err != nil {
				return fmt.Errorf("%s: %w", email, err)
			}
		}
		// Members in the team but absent from the doc → default-deny:
		// drop their existing OrgPermission + NamespacePermission rows.
		// This reaches admins too. Stripping an admin's grants doesn't
		// strip their role, so they can always grant themselves back —
		// the doc governs the four bits, the role governs who may edit
		// the doc.
		for _, m := range members {
			if m.Pending() {
				continue
			}
			if _, listed := doc[m.Email]; listed {
				continue
			}
			uid := m.UserID
			if err := txStore.scoped(orgID).Where("user_id = ?", uid).Delete(&OrgPermission{}).Error; err != nil {
				return err
			}
			if err := reconcileNsPerms(txStore, uid, nil, nsByName); err != nil {
				return fmt.Errorf("%s: %w", m.Email, err)
			}
		}
		return nil
	})
}

// reconcileNsPerms makes the user's NamespacePermission rows match
// `want` exactly: upserts listed namespaces, deletes unlisted ones.
func reconcileNsPerms(s *Store, userID string, want map[string]NamespaceAcl, nsByName map[string]Namespace) error {
	current, err := s.ListUserNamespacePermissions(userID)
	if err != nil {
		return err
	}
	currentByNsID := make(map[uint]bool, len(current))
	for _, np := range current {
		currentByNsID[np.NamespaceID] = true
	}
	wantByNsID := make(map[uint]NamespaceAcl, len(want))
	for name, acl := range want {
		ns := nsByName[name]
		wantByNsID[ns.ID] = acl
	}
	for nsID, acl := range wantByNsID {
		err := s.UpsertNamespacePermission(&NamespacePermission{
			UserID:          userID,
			NamespaceID:     nsID,
			CanRead:         acl.Read,
			CanCreateTokens: acl.CreateTokens,
		})
		if err != nil {
			return err
		}
	}
	for nsID := range currentByNsID {
		if _, keep := wantByNsID[nsID]; !keep {
			if err := s.db.Where("user_id = ? AND namespace_id = ?", userID, nsID).Delete(&NamespacePermission{}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
