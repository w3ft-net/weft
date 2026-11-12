// Package alerts is hosted weft's alert targets: Slack and PagerDuty,
// registered on the coordinator's Notifier beside the webhook and email the core ships.
// Email is the core's, over the product's SMTP settings. Their credentials live in the hosted store (the org's NotifyConfig and
// pagerduty_integrations rows), which is why they are not in the core.
// docs/plans/tenant-scope.md, Phase 4.
package alerts

import (
	"net/http"

	"github.com/graybeardcorp/saaskit/email"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
)

// Targets are the hosted kinds over st, and email over the product's SMTP
// settings (the core's email target, which would otherwise read the
// environment's). emailCfg nil disables email (dev mode): an email entry then
// fails to send, as it always has.
func Targets(st *store.Store, emailCfg *email.Config) []alerting.AlertTarget {
	client := &http.Client{Timeout: alerting.Timeout}
	return []alerting.AlertTarget{
		Slack{Store: st, Client: client},
		PagerDuty{Store: st, Client: client},
		alerting.EmailTarget{SMTP: smtpOf(emailCfg)},
	}
}

func smtpOf(c *email.Config) *alerting.SMTP {
	if c == nil {
		return nil
	}
	return &alerting.SMTP{Host: c.Host, Port: c.Port, User: c.User, Password: c.Password, From: c.From}
}
