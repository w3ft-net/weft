package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kevlarcon/weft/store"
)

// pdEventsURL is the PagerDuty Events v2 endpoint. Overridable in tests.
var pdEventsURL = "https://events.pagerduty.com/v2/enqueue"

// Notifier dispatches alerts through the per-customer alias map stored in
// /etc/notify.conf. A cron watch's `notify:` field names an alias; the alias
// fans out to one or more Slack / PagerDuty targets.
type Notifier struct {
	Store  *store.Store
	client *http.Client
}

// NewNotifier creates a notifier with a default HTTP client.
func NewNotifier(s *store.Store) *Notifier {
	return &Notifier{
		Store:  s,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Send dispatches a notification to the named alias from the customer's
// /etc/notify.conf. watchName is used to build the PagerDuty dedup key so
// a watch firing repeatedly during an outage collapses to one incident —
// ad-hoc shell invocations should pass a fresh synthetic name (see
// AlertContext.dispatchIdentity). body is the alert text; individual
// channels may truncate further to satisfy per-service limits.
func (n *Notifier) Send(customerID uint, aliasName, watchName, body string) error {
	aliasName = strings.TrimSpace(aliasName)
	if aliasName == "" {
		return nil
	}

	nc, err := n.Store.GetNotifyConfig(customerID)
	if err != nil {
		return fmt.Errorf("no notify.conf configured")
	}
	aliases, err := store.ParseNotifyAliases(nc.AliasesYAML)
	if err != nil {
		return fmt.Errorf("parse notify.conf: %w", err)
	}
	alias, ok := aliases[aliasName]
	if !ok {
		return fmt.Errorf("alias %q not defined in /etc/notify.conf", aliasName)
	}

	var firstErr error
	for _, t := range alias.Slack {
		if err := n.sendSlack(nc, watchName, t, body); err != nil {
			log.Printf("notify[%s]: slack %s: %v", watchName, t.Channel, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, t := range alias.PagerDuty {
		if err := n.sendPagerDuty(customerID, watchName, t, body); err != nil {
			log.Printf("notify[%s]: pagerduty %s: %v", watchName, t.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ── Slack ─────────────────────────────────────────────

func (n *Notifier) sendSlack(nc *store.NotifyConfig, watchName string, target store.SlackTarget, stderr string) error {
	if len(stderr) > 1000 {
		stderr = stderr[:1000] + "\n... (truncated)"
	}
	text := fmt.Sprintf(":rotating_light: *weft alert: %s*\n```\n%s\n```", watchName, stderr)

	if nc.SlackBotToken != "" {
		return n.postSlackMessage(nc.SlackBotToken, target.Channel, text, watchName)
	}
	if nc.SlackWebhook != "" {
		return n.postSlackWebhook(nc.SlackWebhook, target.Channel, text, watchName)
	}
	return fmt.Errorf("no Slack credentials configured (connect Slack under Account → Integrations)")
}

func (n *Notifier) postSlackMessage(botToken, channel, text, watchName string) error {
	payload, _ := json.Marshal(map[string]string{
		"channel": channel,
		"text":    text,
	})

	req, _ := http.NewRequest("POST", "https://slack.com/api/chat.postMessage", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack API: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.OK {
		return fmt.Errorf("slack API error: %s", result.Error)
	}

	log.Printf("notify: sent Slack alert for watch %s to %s", watchName, channel)
	return nil
}

func (n *Notifier) postSlackWebhook(webhook, channel, text, watchName string) error {
	payload, _ := json.Marshal(map[string]string{
		"channel": channel,
		"text":    text,
	})

	resp, err := n.client.Post(webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}

	log.Printf("notify: sent Slack alert for watch %s to %s (webhook)", watchName, channel)
	return nil
}

// ── PagerDuty ────────────────────────────────────────

// pagerDutyEvent is the Events v2 payload shape. See
// https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
type pagerDutyEvent struct {
	RoutingKey  string             `json:"routing_key"`
	EventAction string             `json:"event_action"` // "trigger" | "acknowledge" | "resolve"
	DedupKey    string             `json:"dedup_key,omitempty"`
	Payload     pagerDutyPayload   `json:"payload"`
	Client      string             `json:"client,omitempty"`
	ClientURL   string             `json:"client_url,omitempty"`
}

type pagerDutyPayload struct {
	Summary   string         `json:"summary"`
	Source    string         `json:"source"`
	Severity  string         `json:"severity"`
	Component string         `json:"component,omitempty"`
	Group     string         `json:"group,omitempty"`
	Class     string         `json:"class,omitempty"`
	Details   map[string]any `json:"custom_details,omitempty"`
}

func (n *Notifier) sendPagerDuty(customerID uint, watchName string, target store.PDTarget, stderr string) error {
	integ, err := n.Store.GetPagerDutyIntegration(customerID, target.Name)
	if err != nil {
		return fmt.Errorf("no integration named %q (configure under Account → Integrations)", target.Name)
	}

	summary := watchName
	if len(stderr) > 0 {
		first := stderr
		if idx := strings.IndexByte(first, '\n'); idx > 0 {
			first = first[:idx]
		}
		if len(first) > 120 {
			first = first[:120] + "…"
		}
		summary = fmt.Sprintf("%s — %s", watchName, first)
	}

	evt := pagerDutyEvent{
		RoutingKey:  integ.IntegrationKey,
		EventAction: "trigger",
		DedupKey:    fmt.Sprintf("weft:%d:%s", customerID, watchName),
		Client:      "weft",
		Payload: pagerDutyPayload{
			Summary:  summary,
			Source:   "weft",
			Severity: target.Severity,
			Class:    "watch",
			Details: map[string]any{
				"watch":  watchName,
				"stderr": truncate(stderr, 4000),
			},
		},
	}

	body, _ := json.Marshal(evt)
	resp, err := n.client.Post(pdEventsURL, "application/json", bytes.NewReader(body))
	if err != nil {
		n.Store.RecordPagerDutySend(customerID, target.Name, err.Error())
		return fmt.Errorf("pagerduty POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		n.Store.RecordPagerDutySend(customerID, target.Name, fmt.Sprintf("http %d", resp.StatusCode))
		return fmt.Errorf("pagerduty returned %d", resp.StatusCode)
	}
	n.Store.RecordPagerDutySend(customerID, target.Name, "")
	log.Printf("notify: sent PagerDuty alert for watch %s to integration %s (severity %s)",
		watchName, target.Name, target.Severity)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}
