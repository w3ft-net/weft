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

// PagerDutyTarget triggers an incident through the Events v2 API. The routing
// key is the secret `pagerduty_<name>`, or the one the entry's `token:` names.
type PagerDutyTarget struct {
	secrets SecretResolver
	client  *http.Client
	// EventsURL is the Events v2 endpoint; tests point it elsewhere.
	EventsURL string
}

// NewPagerDutyTarget is the pagerduty target over secrets.
func NewPagerDutyTarget(secrets SecretResolver) *PagerDutyTarget {
	return &PagerDutyTarget{secrets: secrets, client: &http.Client{Timeout: Timeout}, EventsURL: "https://events.pagerduty.com/v2/enqueue"}
}

func (*PagerDutyTarget) Kind() string { return "pagerduty" }

func (p *PagerDutyTarget) Open(entry *yaml.Node) (Destination, error) {
	var t notifycfg.PDTarget
	if err := entry.Decode(&t); err != nil {
		return Destination{}, err
	}
	if t.Severity == "" {
		t.Severity = "error"
	}
	secret := notifycfg.TokenSecret(t.Token, "pagerduty_"+t.Name)
	return Destination{
		Key:  "pagerduty:" + t.Name,
		Name: t.Name,
		Send: func(ctx context.Context, a Alert) error {
			key, err := p.secrets.Secret(a.Owner, secret)
			if err != nil {
				return fmt.Errorf("pagerduty routing key: %w", err)
			}
			return p.send(ctx, key, a, t)
		},
	}, nil
}

// pagerDutyEvent is the Events v2 payload shape. See
// https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
type pagerDutyEvent struct {
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"`
	DedupKey    string           `json:"dedup_key,omitempty"`
	Payload     pagerDutyPayload `json:"payload"`
	Client      string           `json:"client,omitempty"`
}

type pagerDutyPayload struct {
	Summary  string         `json:"summary"`
	Source   string         `json:"source"`
	Severity string         `json:"severity"`
	Class    string         `json:"class,omitempty"`
	Details  map[string]any `json:"custom_details,omitempty"`
}

func (p *PagerDutyTarget) send(ctx context.Context, routingKey string, a Alert, t notifycfg.PDTarget) error {
	summary := a.Cron
	if a.Body != "" {
		first := a.Body
		if idx := strings.IndexByte(first, '\n'); idx > 0 {
			first = first[:idx]
		}
		if len(first) > 120 {
			first = first[:120] + "…"
		}
		summary = fmt.Sprintf("%s — %s", a.Cron, first)
	}
	evt := pagerDutyEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		// One incident per cron: a repeat of the same alert updates it.
		DedupKey: fmt.Sprintf("weft:%v:%s", a.Owner, a.Cron),
		Client:   "weft",
		Payload: pagerDutyPayload{
			Summary:  summary,
			Source:   "weft",
			Severity: t.Severity,
			Class:    "cron",
			Details:  map[string]any{"cron": a.Cron, "stderr": Truncate(a.Body, 4000)},
		},
	}
	body, _ := json.Marshal(evt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.EventsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pagerduty POST: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pagerduty returned %d", resp.StatusCode)
	}
	weftsdk.Infof(ctx, "notify: sent PagerDuty alert for cron %s to %s (severity %s)", a.Cron, t.Name, t.Severity)
	return nil
}
