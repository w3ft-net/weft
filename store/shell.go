package store

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// ParseScheduleInterval parses a schedule string into a duration.
// Supports "every <duration>" (e.g., "every 1m") and basic cron
// shorthand "*/N * * * *" (every N minutes). Returns 5m for
// unrecognized formats.
func ParseScheduleInterval(schedule string) time.Duration {
	if strings.HasPrefix(schedule, "every ") {
		if d, err := time.ParseDuration(strings.TrimPrefix(schedule, "every ")); err == nil {
			return d
		}
	}

	if strings.HasPrefix(schedule, "*/") {
		parts := strings.Fields(schedule)
		if len(parts) >= 1 {
			nStr := strings.TrimPrefix(parts[0], "*/")
			n := 0
			for _, ch := range nStr {
				if ch >= '0' && ch <= '9' {
					n = n*10 + int(ch-'0')
				} else {
					break
				}
			}
			if n > 0 {
				return time.Duration(n) * time.Minute
			}
		}
	}

	return 5 * time.Minute
}

// ShellFunction is a user-defined function stored in the virtual filesystem.
// Language is detected from shebang at write time: "bash" (default) or "lua".
type ShellFunction struct {
	gorm.Model
	CustomerID uint   `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Source     string `gorm:"type:text;not null"`
	Language   string `gorm:"not null;default:bash"` // "bash" or "lua"
}

func (s *Store) SaveShellFunction(customerID uint, name, source string) error {
	lang, cleanSource := detectLanguage(source)
	var fn ShellFunction
	result := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&fn)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&ShellFunction{
			CustomerID: customerID, Name: name,
			Source: cleanSource, Language: lang,
		}).Error
	}
	return s.db.Model(&fn).Updates(map[string]any{
		"source":   cleanSource,
		"language": lang,
	}).Error
}

// detectLanguage checks for a Lua shebang and returns the language
// and source with the shebang stripped.
func detectLanguage(source string) (lang, cleanSource string) {
	firstLine, rest, _ := strings.Cut(source, "\n")
	trimmed := strings.TrimSpace(firstLine)
	if strings.HasPrefix(trimmed, "#!") && strings.Contains(trimmed, "lua") {
		return "lua", rest
	}
	return "bash", source
}

func (s *Store) GetShellFunction(customerID uint, name string) (*ShellFunction, error) {
	var fn ShellFunction
	err := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&fn).Error
	if err != nil {
		return nil, err
	}
	return &fn, nil
}

func (s *Store) ListShellFunctions(customerID uint) ([]ShellFunction, error) {
	var fns []ShellFunction
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&fns).Error
	return fns, err
}

func (s *Store) DeleteShellFunction(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&ShellFunction{}).Error
}

// --- Cron Watches (/cron.d/) ---

// CronWatch is a scheduled pipeline run.
type CronWatch struct {
	gorm.Model
	CustomerID uint   `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Schedule   string `gorm:"not null"`        // "*/5 * * * *" or "every 1m"
	Pipeline   string `gorm:"type:text;not null"`
	NotifyDest string                           // "slack:#ops"
	Enabled    bool   `gorm:"not null;default:true"`
	LastRunAt  *time.Time
	LastStatus string                           // "ok", "error", "stderr"
}

func (s *Store) SaveCronWatch(customerID uint, name, schedule, pipeline, notifyDest string) error {
	var w CronWatch
	result := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&w)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&CronWatch{
			CustomerID: customerID,
			Name:       name,
			Schedule:   schedule,
			Pipeline:   pipeline,
			NotifyDest: notifyDest,
			Enabled:    true,
		}).Error
	}
	return s.db.Model(&w).Updates(map[string]any{
		"schedule":    schedule,
		"pipeline":    pipeline,
		"notify_dest": notifyDest,
	}).Error
}

func (s *Store) GetCronWatch(customerID uint, name string) (*CronWatch, error) {
	var w CronWatch
	err := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&w).Error
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListCronWatches(customerID uint) ([]CronWatch, error) {
	var watches []CronWatch
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&watches).Error
	return watches, err
}

func (s *Store) ListEnabledCronWatches() ([]CronWatch, error) {
	var watches []CronWatch
	err := s.db.Where("enabled = ?", true).Find(&watches).Error
	return watches, err
}

func (s *Store) DeleteCronWatch(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&CronWatch{}).Error
}

// CountCronWatches returns the number of cron watches for a customer.
func (s *Store) CountCronWatches(customerID uint) (int, error) {
	var count int64
	err := s.db.Model(&CronWatch{}).Where("customer_id = ?", customerID).Count(&count).Error
	return int(count), err
}

func (s *Store) UpdateCronWatchRun(id uint, status string) error {
	now := time.Now()
	return s.db.Model(&CronWatch{}).Where("id = ?", id).Updates(map[string]any{
		"last_run_at": &now,
		"last_status": status,
	}).Error
}

// --- Cron Results ---

// CronResult stores the output of a completed cron watch run.
type CronResult struct {
	gorm.Model
	CronWatchID uint      `gorm:"not null;index"`
	RanAt       time.Time `gorm:"not null"`
	Stdout      string    `gorm:"type:text"`
	Stderr      string    `gorm:"type:text"`
	ExitCode    int
	DurationMs  int64
}

func (s *Store) SaveCronResult(result *CronResult) error {
	return s.db.Create(result).Error
}

func (s *Store) ListCronResults(cronWatchID uint, limit int) ([]CronResult, error) {
	var results []CronResult
	err := s.db.Where("cron_watch_id = ?", cronWatchID).
		Order("ran_at DESC").Limit(limit).Find(&results).Error
	return results, err
}

// QueryCronResults returns results for a named watch within a time range,
// scoped to a customer. Used by weft.* functions in /bin/ scripts.
func (s *Store) QueryCronResults(customerID uint, watchName string, since time.Time) ([]CronResult, error) {
	var watch CronWatch
	if err := s.db.Where("customer_id = ? AND name = ?", customerID, watchName).First(&watch).Error; err != nil {
		return nil, err
	}
	var results []CronResult
	err := s.db.Where("cron_watch_id = ? AND ran_at >= ?", watch.ID, since).
		Order("ran_at DESC").Find(&results).Error
	return results, err
}

// LatestCronResult returns the most recent result for a named watch.
func (s *Store) LatestCronResult(customerID uint, watchName string) (*CronResult, error) {
	var watch CronWatch
	if err := s.db.Where("customer_id = ? AND name = ?", customerID, watchName).First(&watch).Error; err != nil {
		return nil, err
	}
	var result CronResult
	err := s.db.Where("cron_watch_id = ?", watch.ID).Order("ran_at DESC").First(&result).Error
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Notification Config ---

// NotifyConfig stores notification channel configuration per customer.
// SlackWebhook comes from OAuth. PagerDutyKeys and Routes are JSON maps
// serialized as YAML in /etc/notify.conf.
type NotifyConfig struct {
	gorm.Model
	CustomerID    uint       `gorm:"not null;uniqueIndex"`
	SlackWebhook  string     // incoming webhook URL (legacy, may be empty)
	SlackBotToken string     // bot OAuth token for chat.postMessage
	SlackTeamName string     // workspace name (for display)
	PagerDutyKeys string     `gorm:"type:text"` // JSON: {"api": "key1", "workers": "key2"}
	Routes        string     `gorm:"type:text"` // JSON: {"error-check": "slack:#ops", ...}
	ConfiguredAt  *time.Time
}

func (s *Store) GetNotifyConfig(customerID uint) (*NotifyConfig, error) {
	var nc NotifyConfig
	err := s.db.Where("customer_id = ?", customerID).First(&nc).Error
	if err != nil {
		return nil, err
	}
	return &nc, nil
}

func (s *Store) SaveNotifyConfig(customerID uint, slackWebhook string) error {
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&NotifyConfig{CustomerID: customerID, SlackWebhook: slackWebhook}).Error
	}
	return s.db.Model(&nc).Update("slack_webhook", slackWebhook).Error
}

func (s *Store) SaveSlackWebhook(customerID uint, webhook, teamName string) error {
	return s.SaveSlackConfig(customerID, webhook, "", teamName)
}

func (s *Store) SaveSlackConfig(customerID uint, webhook, botToken, teamName string) error {
	now := time.Now()
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&NotifyConfig{
			CustomerID:    customerID,
			SlackWebhook:  webhook,
			SlackBotToken: botToken,
			SlackTeamName: teamName,
			ConfiguredAt:  &now,
		}).Error
	}
	return s.db.Model(&nc).Updates(map[string]any{
		"slack_webhook":   webhook,
		"slack_bot_token": botToken,
		"slack_team_name": teamName,
		"configured_at":   &now,
	}).Error
}

func (s *Store) DisconnectSlack(customerID uint) error {
	return s.db.Model(&NotifyConfig{}).Where("customer_id = ?", customerID).Updates(map[string]any{
		"slack_webhook":   "",
		"slack_team_name": "",
	}).Error
}

func (s *Store) SavePagerDutyKeys(customerID uint, keysJSON string) error {
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		now := time.Now()
		return s.db.Create(&NotifyConfig{
			CustomerID:    customerID,
			PagerDutyKeys: keysJSON,
			ConfiguredAt:  &now,
		}).Error
	}
	return s.db.Model(&nc).Update("pager_duty_keys", keysJSON).Error
}

func (s *Store) SaveNotifyRoutes(customerID uint, routesJSON string) error {
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&NotifyConfig{
			CustomerID: customerID,
			Routes:     routesJSON,
		}).Error
	}
	return s.db.Model(&nc).Update("routes", routesJSON).Error
}
