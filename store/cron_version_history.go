package store

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// AmendCronWatchMessage sets the commit message on a cron's current version.
// It is a plain update of the live row, so it does not capture history.
func (s *Store) AmendCronWatchMessage(orgID OrgID, ns uint, authorID, name, message string) error {
	q := inCronHome(s.scoped(orgID).Model(&CronWatch{}), ns).Where("name = ?", name)
	return amendMessage(q, authorID, message)
}

// ResolveCronPath splits a cron's path below /cron.d into its home namespace
// (0 for the org level) and its name: "errors" is org-level, "alpha/errors" is
// errors in alpha. The bare name of a namespace is not a cron.
func (s *Store) ResolveCronPath(orgID OrgID, rel string) (ns uint, name string, err error) {
	home, name, homed := strings.Cut(rel, "/")
	if !homed {
		return 0, rel, nil
	}
	n, err := s.GetNamespace(orgID, home)
	if err != nil || name == "" || strings.Contains(name, "/") {
		return 0, "", fmt.Errorf("/cron.d/%s: no such cron: %w", rel, gorm.ErrRecordNotFound)
	}
	return n.ID, name, nil
}

// ResolveCronRef resolves a reference to a cron (from a weft:: function, a
// dashboard tile or a script) to its home and name. A reference always names
// the namespace: "alpha/errors". A bare name is an error, even when an
// org-level cron has it, since org-level crons are not referenced.
func (s *Store) ResolveCronRef(orgID OrgID, ref string) (ns uint, name string, err error) {
	if !strings.Contains(ref, "/") {
		return 0, "", fmt.Errorf("a cron is named by its namespace and name, <namespace>/<name>; %q has no namespace", ref)
	}
	return s.ResolveCronPath(orgID, ref)
}

// GetCronAt finds the cron a reference names.
func (s *Store) GetCronAt(orgID OrgID, ref string) (*CronWatch, error) {
	ns, name, err := s.ResolveCronRef(orgID, ref)
	if err != nil {
		return nil, err
	}
	return s.GetCronWatch(orgID, ns, name)
}
