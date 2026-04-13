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

// Notifier sends alerts when cron watches produce stderr output.
type Notifier struct {
	Store  *store.Store
	client *http.Client
}

// NewNotifier creates a notifier with a default HTTP client.
func NewNotifier(store *store.Store) *Notifier {
	return &Notifier{
		Store:  store,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Send dispatches a notification for a watch that produced stderr.
// The notifyDest string determines the channel (e.g., "slack:#ops").
func (n *Notifier) Send(customerID uint, watch *store.CronWatch, stderr string) error {
	dest := watch.NotifyDest
	if dest == "" {
		return nil
	}

	if strings.HasPrefix(dest, "slack:") {
		channel := strings.TrimPrefix(dest, "slack:")
		return n.sendSlack(customerID, watch.Name, channel, stderr)
	}

	log.Printf("notify: unknown destination type %q for watch %s", dest, watch.Name)
	return fmt.Errorf("unknown notification destination: %s", dest)
}

// sendSlack posts an alert to a Slack channel via the Slack API (chat.postMessage).
func (n *Notifier) sendSlack(customerID uint, watchName, channel, stderr string) error {
	nc, err := n.Store.GetNotifyConfig(customerID)
	if err != nil {
		return fmt.Errorf("no Slack configuration found")
	}

	// Truncate stderr if very long.
	if len(stderr) > 1000 {
		stderr = stderr[:1000] + "\n... (truncated)"
	}

	text := fmt.Sprintf(":rotating_light: *weft alert: %s*\n```\n%s\n```", watchName, stderr)

	// Prefer bot token (chat:write scope, any channel).
	if nc.SlackBotToken != "" {
		return n.postSlackMessage(nc.SlackBotToken, channel, text, watchName)
	}

	// Fallback to webhook (legacy, single-channel).
	if nc.SlackWebhook != "" {
		return n.postSlackWebhook(nc.SlackWebhook, channel, text, watchName)
	}

	return fmt.Errorf("no Slack token or webhook configured")
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
