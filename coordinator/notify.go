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

// sendSlack posts an alert to a Slack channel via the customer's webhook.
func (n *Notifier) sendSlack(customerID uint, watchName, channel, stderr string) error {
	nc, err := n.Store.GetNotifyConfig(customerID)
	if err != nil || nc.SlackWebhook == "" {
		return fmt.Errorf("no Slack webhook configured")
	}

	// Truncate stderr if very long.
	if len(stderr) > 1000 {
		stderr = stderr[:1000] + "\n... (truncated)"
	}

	payload := slackPayload{
		Channel: channel,
		Text:    fmt.Sprintf(":rotating_light: *weft alert: %s*\n```\n%s\n```", watchName, stderr),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	resp, err := n.client.Post(nc.SlackWebhook, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}

	log.Printf("notify: sent Slack alert for watch %s to %s", watchName, channel)
	return nil
}

type slackPayload struct {
	Channel string `json:"channel,omitempty"`
	Text    string `json:"text"`
}
