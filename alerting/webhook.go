package alerting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/notifycfg"
)

// scrubbedError reports msg in place of err's text, keeping err for errors.Is.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// webhookPayload is the versioned JSON shape sent to generic webhook targets.
// v2 renamed `watch` to `cron`.
type webhookPayload struct {
	V      int    `json:"v"`
	Watch  string `json:"cron"`
	Alias  string `json:"alias"`
	TsUnix int64  `json:"ts_unix"`
	Body   string `json:"body"`
}

// WebhookTarget is the core's alert target: a JSON POST to a URL, with
// ${secrets.NAME} references in the URL and headers filled in at send time.
type WebhookTarget struct {
	secrets SecretResolver
	client  *http.Client
}

// NewWebhookTarget is the webhook target over secrets, which fills in the
// ${secrets.NAME} references.
func NewWebhookTarget(secrets SecretResolver) *WebhookTarget {
	return &WebhookTarget{secrets: secrets, client: &http.Client{Timeout: Timeout}}
}

func (*WebhookTarget) Kind() string { return "webhook" }

func (w *WebhookTarget) Open(entry *yaml.Node) (Destination, error) {
	var t notifycfg.WebhookTarget
	if err := entry.Decode(&t); err != nil {
		return Destination{}, err
	}
	// The URL can carry a secret, so the stored key is a digest of it. It is
	// taken before substitution, from the ${secrets.NAME} text.
	sum := sha256.Sum256([]byte(t.URL))
	return Destination{
		Key:  "webhook:" + hex.EncodeToString(sum[:6]),
		Name: t.URL,
		Send: func(ctx context.Context, a Alert) error {
			resolved, mask, err := w.secrets.ResolveWebhook(a.Owner, t)
			if err != nil {
				return err
			}
			if err := w.send(ctx, a, resolved); err != nil {
				// An HTTP client's error carries the whole URL.
				return &scrubbedError{msg: mask.Replace(err.Error()), err: err}
			}
			return nil
		},
	}, nil
}

func (w *WebhookTarget) send(ctx context.Context, a Alert, target notifycfg.WebhookTarget) error {
	payload, _ := json.Marshal(webhookPayload{
		V:      2,
		Watch:  a.Cron,
		Alias:  a.Alias,
		TsUnix: time.Now().Unix(),
		Body:   Truncate(a.Body, 4000),
	})

	req, err := http.NewRequestWithContext(ctx, "POST", target.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}

	weftsdk.Infof(ctx, "notify: sent webhook alert for watch %s", a.Cron)
	return nil
}
