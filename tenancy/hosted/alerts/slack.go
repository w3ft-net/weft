package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
)

// Slack posts to a channel with the org's bot token, or its incoming webhook
// for a tenant that never re-authed.
type Slack struct {
	Store  *store.Store
	Client *http.Client
}

func (Slack) Kind() string { return "slack" }

func (s Slack) Open(entry *yaml.Node) (alerting.Destination, error) {
	var t store.SlackTarget
	if err := entry.Decode(&t); err != nil {
		return alerting.Destination{}, err
	}
	return alerting.Destination{
		Key:  "slack:" + t.Channel,
		Name: t.Channel,
		Send: func(ctx context.Context, a alerting.Alert) error {
			nc, err := s.Store.GetNotifyConfig(a.Org)
			if err != nil {
				return fmt.Errorf("no notify.yaml configured")
			}
			return s.send(ctx, nc, a.Cron, t, a.Body)
		},
	}, nil
}

func (s Slack) send(ctx context.Context, nc *store.NotifyConfig, watchName string, target store.SlackTarget, stderr string) error {
	if len(stderr) > 1000 {
		stderr = stderr[:1000] + "\n... (truncated)"
	}
	text := fmt.Sprintf(":rotating_light: *weft alert: %s*\n```\n%s\n```", watchName, stderr)

	if nc.SlackBotToken != "" {
		return s.postMessage(ctx, nc.SlackBotToken, target.Channel, text, watchName)
	}
	if nc.SlackWebhook != "" {
		return s.postWebhook(ctx, nc.SlackWebhook, target.Channel, text, watchName)
	}
	return fmt.Errorf("no Slack credentials configured (connect Slack under Account → Integrations)")
}

func (s Slack) postMessage(ctx context.Context, botToken, channel, text, watchName string) error {
	payload, _ := json.Marshal(map[string]string{
		"channel": channel,
		"text":    text,
	})

	req, _ := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/chat.postMessage", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := s.Client.Do(req)
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

	weftsdk.Infof(ctx, "notify: sent Slack alert for watch %s to %s", watchName, channel)
	return nil
}

func (s Slack) postWebhook(ctx context.Context, webhook, channel, text, watchName string) error {
	payload, _ := json.Marshal(map[string]string{
		"channel": channel,
		"text":    text,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("slack webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}

	weftsdk.Infof(ctx, "notify: sent Slack alert for watch %s to %s (webhook)", watchName, channel)
	return nil
}
