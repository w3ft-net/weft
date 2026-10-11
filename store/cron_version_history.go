package store

// AmendCronWatchMessage sets the commit message on a cron's current version.
// It is a plain update of the live row, so it does not capture history.
func (s *Store) AmendCronWatchMessage(orgID OrgID, name, message string) error {
	return s.scoped(orgID).Model(&CronWatch{}).
		Where("name = ?", name).
		Update("message", message).Error
}
