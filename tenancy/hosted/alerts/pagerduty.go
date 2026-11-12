package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
)

// EventsURL is the PagerDuty Events v2 endpoint. Overridable in tests.
var EventsURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDuty triggers an incident with a named integration key of the org.
type PagerDuty struct {
	Store  *store.Store
	Client *http.Client
}

func (PagerDuty) Kind() string { return "pagerduty" }

func (p PagerDuty) Open(entry *yaml.Node) (alerting.Destination, error) {
	var t store.PDTarget
	if err := entry.Decode(&t); err != nil {
		return alerting.Destination{}, err
	}
	if t.Severity == "" {
		t.Severity = "error"
	}
	return alerting.Destination{
		Key:  "pagerduty:" + t.Name,
		Name: t.Name,
		Send: func(ctx context.Context, a alerting.Alert) error { return p.send(ctx, a.Org, a.Cron, t, a.Body) },
	}, nil
}

// pagerDutyEvent is the Events v2 payload shape. See
// https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
type pagerDutyEvent struct {
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"` // "trigger" | "acknowledge" | "resolve"
	DedupKey    string           `json:"dedup_key,omitempty"`
	Payload     pagerDutyPayload `json:"payload"`
	Client      string           `json:"client,omitempty"`
	ClientURL   string           `json:"client_url,omitempty"`
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

func (p PagerDuty) send(ctx context.Context, orgID store.OrgID, watchName string, target store.PDTarget, stderr string) error {
	integ, err := func() (*store.PagerDutyIntegration, error) {
		var item store.PagerDutyIntegration
		err := p.Store.DB().WithContext(store.WithOrg(ctx, orgID)).Where("name = ?", target.Name).First(&item).Error
		if err != nil {
			return nil, err
		}
		return &item, nil
	}()
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
		DedupKey:    fmt.Sprintf("weft:%s:%s", orgID, watchName),
		Client:      "weft",
		Payload: pagerDutyPayload{
			Summary:  summary,
			Source:   "weft",
			Severity: target.Severity,
			Class:    "cron",
			Details: map[string]any{
				"cron":   watchName,
				"stderr": alerting.Truncate(stderr, 4000),
			},
		},
	}

	body, _ := json.Marshal(evt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EventsURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pagerduty POST: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		p.Store.RecordPagerDutySend(orgID, target.Name, err.Error())
		return fmt.Errorf("pagerduty POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		p.Store.RecordPagerDutySend(orgID, target.Name, fmt.Sprintf("http %d", resp.StatusCode))
		return fmt.Errorf("pagerduty returned %d", resp.StatusCode)
	}
	p.Store.RecordPagerDutySend(orgID, target.Name, "")
	weftsdk.Infof(ctx, "notify: sent PagerDuty alert for watch %s to integration %s (severity %s)",
		watchName, target.Name, target.Severity)
	return nil
}
