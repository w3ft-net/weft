package store

import (
	"fmt"
	"sort"
	"strings"
)

// Each namespace x has a system user, x-cron: the identity its crons fire as.
// It is not a team member and has no login; it is a subject in
// namespace_permissions like any member, so what it may read is whatever
// groups it holds (docs/plans/tenant-scope.md, "Groups and the cron user").

const cronUserSuffix = "-cron"

// CronUserID is the subject of namespace nsID's cron user.
func CronUserID(nsID uint) string { return fmt.Sprintf("cron:%d", nsID) }

// cronUserNamespace resolves "x-cron" to namespace x.
func (s *Store) cronUserNamespace(orgID OrgID, user string) (*Namespace, error) {
	name, ok := strings.CutSuffix(user, cronUserSuffix)
	if !ok || name == "" {
		return nil, fmt.Errorf("no such user: %s", user)
	}
	ns, err := s.GetNamespace(orgID, name)
	if err != nil {
		return nil, fmt.Errorf("no such user: %s", user)
	}
	return ns, nil
}

// IsCronUser reports whether user names a namespace's cron user.
func (s *Store) IsCronUser(orgID OrgID, user string) bool {
	_, err := s.cronUserNamespace(orgID, user)
	return err == nil
}

// SetCronUserGroup adds user (a cron user, "x-cron") to a read group "y-r", or
// removes it. Nothing needs a cron user to hold a write group, so only read
// groups of existing namespaces are accepted.
func (s *Store) SetCronUserGroup(orgID OrgID, user, group string, grant bool) error {
	home, err := s.cronUserNamespace(orgID, user)
	if err != nil {
		return err
	}
	name, write, ok := SplitNamespaceGroup(group)
	if !ok || write {
		return fmt.Errorf("%s can only be added to a read group (<namespace>-r)", user)
	}
	target, err := s.findNamespace(orgID, name)
	if err != nil {
		return err
	}
	return s.setNamespaceBit(CronUserID(home.ID), target.ID, false, grant)
}

// CronUserGroups lists the read groups of a cron user, sorted.
func (s *Store) CronUserGroups(orgID OrgID, user string) ([]string, error) {
	home, err := s.cronUserNamespace(orgID, user)
	if err != nil {
		return nil, err
	}
	perms, err := s.ListUserNamespacePermissions(CronUserID(home.ID))
	if err != nil {
		return nil, err
	}
	byID, err := s.namespacesByID(orgID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range perms {
		if name, ok := byID[p.NamespaceID]; ok && p.CanRead {
			out = append(out, name+readSuffix)
		}
	}
	sort.Strings(out)
	return out, nil
}

// seedCronUser puts a new namespace's cron user in its own read group.
func (s *Store) seedCronUser(ns *Namespace) error {
	return s.UpsertNamespacePermission(&NamespacePermission{UserID: CronUserID(ns.ID), NamespaceID: ns.ID, CanRead: true})
}

// CronUserHome is the namespace a cron user ("x-cron") belongs to.
func (s *Store) CronUserHome(orgID OrgID, user string) (string, bool) {
	ns, err := s.cronUserNamespace(orgID, user)
	if err != nil {
		return "", false
	}
	return ns.Name, true
}
