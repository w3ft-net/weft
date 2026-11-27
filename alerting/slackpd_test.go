package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
)

// secretMap is a SecretResolver over a map.
type secretMap map[string]string

func (m secretMap) Secret(_ shape.Added, name string) (string, error) {
	if v, ok := m[name]; ok {
		return v, nil
	}
	return "", &missingSecret{name}
}

type missingSecret struct{ name string }

func (e *missingSecret) Error() string { return "secret " + e.name + " is not defined" }

func (secretMap) ResolveWebhook(_ shape.Added, t notifycfg.WebhookTarget) (notifycfg.WebhookTarget, *strings.Replacer, error) {
	return t, strings.NewReplacer(), nil
}

func entry(t *testing.T, doc string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatal(err)
	}
	return n.Content[0]
}

func TestSlack_BotTokenFromTheDefaultSecret(t *testing.T) {
	var gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	s := NewSlackTarget(secretMap{"slack_token": "xoxb-1"})
	s.APIURL = srv.URL
	d, err := s.Open(entry(t, `"#ops"`))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), Alert{Cron: "disk", Body: "full"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer xoxb-1" || gotBody["channel"] != "#ops" || !strings.Contains(gotBody["text"], "disk") {
		t.Fatalf("auth=%q body=%v", gotAuth, gotBody)
	}
}

func TestSlack_WebhookURLAndNamedSecret(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}))
	defer srv.Close()
	// An incoming-webhook URL is recognized by its scheme; the test server is http, so
	// exercise the webhook path directly.
	s := NewSlackTarget(nil)
	if err := s.postWebhook(context.Background(), srv.URL, "#ops", "hi", "c"); err != nil || !strings.Contains(got, "#ops") {
		t.Fatalf("postWebhook: %v, body %q", err, got)
	}
	d, _ := NewSlackTarget(secretMap{}).Open(entry(t, "{channel: '#ops', token: '${secrets.team_slack}'}"))
	err := d.Send(context.Background(), Alert{Cron: "c"})
	if err == nil || !strings.Contains(err.Error(), "team_slack") {
		t.Fatalf("a missing named secret: err = %v, want it to name team_slack", err)
	}
}

func TestPagerDuty_RoutingKeyFromPagerdutyName(t *testing.T) {
	var evt pagerDutyEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&evt)
		w.WriteHeader(202)
	}))
	defer srv.Close()
	p := NewPagerDutyTarget(secretMap{"pagerduty_primary": "rk-1"})
	p.EventsURL = srv.URL
	d, err := p.Open(entry(t, "{name: primary, severity: critical}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Send(context.Background(), Alert{Cron: "disk", Body: "line one\nline two"}); err != nil {
		t.Fatal(err)
	}
	if evt.RoutingKey != "rk-1" || evt.Payload.Severity != "critical" || evt.DedupKey == "" ||
		!strings.HasSuffix(evt.Payload.Summary, "line one") {
		t.Fatalf("event = %+v", evt)
	}
}
