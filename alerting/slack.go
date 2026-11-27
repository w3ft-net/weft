package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/notifycfg"
)

// DefaultSlackSecret is the secret a slack entry reads its credential from when
// it names none.
const DefaultSlackSecret = "slack_token"

// SlackTarget posts to a channel. The credential is a secret: a bot token
// (chat.postMessage) or an incoming-webhook URL (a value that starts with
// https://), `slack_token` unless the entry's `token:` names another.
type SlackTarget struct {
	secrets SecretResolver
	client  *http.Client
	// APIURL is Slack's chat.postMessage endpoint; tests point it elsewhere.
	APIURL string
}

// NewSlackTarget is the slack target over secrets.
func NewSlackTarget(secrets SecretResolver) *SlackTarget {
	return &SlackTarget{secrets: secrets, client: &http.Client{Timeout: Timeout}, APIURL: "https://slack.com/api/chat.postMessage"}
}

func (*SlackTarget) Kind() string { return "slack" }

func (s *SlackTarget) Open(entry *yaml.Node) (Destination, error) {
	var t notifycfg.SlackTarget
	if err := entry.Decode(&t); err != nil {
		return Destination{}, err
	}
	secret := notifycfg.TokenSecret(t.Token, DefaultSlackSecret)
	return Destination{
		Key:  "slack:" + t.Channel,
		Name: t.Channel,
		Send: func(ctx context.Context, a Alert) error {
			cred, err := s.secrets.Secret(a.Owner, secret)
			if err != nil {
				return fmt.Errorf("slack credential: %w", err)
			}
			text := fmt.Sprintf(":rotating_light: *weft alert: %s*\n```\n%s\n```", a.Cron, Truncate(a.Body, 1000))
			if strings.HasPrefix(cred, "https://") {
				return s.postWebhook(ctx, cred, t.Channel, text, a.Cron)
			}
			return s.postMessage(ctx, cred, t.Channel, text, a.Cron)
		},
	}, nil
}

func (s *SlackTarget) postMessage(ctx context.Context, botToken, channel, text, cron string) error {
	payload, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack API: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+botToken)
	resp, err := s.client.Do(req)
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
	weftsdk.Infof(ctx, "notify: sent Slack alert for cron %s to %s", cron, channel)
	return nil
}

func (s *SlackTarget) postWebhook(ctx context.Context, webhook, channel, text, cron string) error {
	payload, _ := json.Marshal(map[string]string{"channel": channel, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack webhook: %w", scrub(err, webhook))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		// An HTTP client's error carries the whole URL, which is the credential.
		return fmt.Errorf("slack webhook: %w", scrub(err, webhook))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	weftsdk.Infof(ctx, "notify: sent Slack alert for cron %s to %s (webhook)", cron, channel)
	return nil
}

// scrub is err with secret replaced, so a credential in a URL stays out of logs.
func scrub(err error, secret string) error {
	return &scrubbedError{msg: strings.ReplaceAll(err.Error(), secret, "<redacted>"), err: err}
}
