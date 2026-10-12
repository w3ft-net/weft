package store

// AmendCronWatchMessage sets the commit message on a cron's current version.
// It is a plain update of the live row, so it does not capture history.
func (s *Store) AmendCronWatchMessage(orgID OrgID, authorID, name, message string) error {
	q := s.scoped(orgID).Model(&CronWatch{}).Where("name = ?", name)
	return amendMessage(q, authorID, message)
}
